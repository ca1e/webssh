package transfer

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// 任务状态
const (
	StatusRunning  = "running"
	StatusDone     = "done"
	StatusFailed   = "failed"
	StatusCanceled = "canceled"
)

// 并发与缓冲参数
const (
	workersPerTask = 4       // 单任务并发 worker 数(pkg/sftp 单连接并发安全)
	globalWorkers  = 8       // 全局并发拷贝上限(避免多任务叠加打满链路)
	copyBufSize    = 1 << 20 // 1MB 缓冲
	maxKeptTasks   = 200     // 任务记录上限(超出时淘汰最早的已结束任务)
)

var errCanceled = errors.New("已取消")

// Endpoint 传输端点。
type Endpoint struct {
	Kind string `json:"kind"` // session: 远程;local: 本机
	SID  string `json:"sid,omitempty"`
	Path string `json:"path"`
}

// TaskView 任务快照(JSON 输出),字段与旧 RelayTask 契约保持兼容。
type TaskView struct {
	ID         string    `json:"id"`
	SourceName string    `json:"source_name"`
	TargetName string    `json:"target_name"`
	SourceKind string    `json:"source_kind,omitempty"` // session | local
	TargetKind string    `json:"target_kind,omitempty"` // session | local
	TargetSID  string    `json:"target_sid,omitempty"`  // 目标为会话时的会话 id,前端刷新对端列表用
	SourcePath string    `json:"source_path"`
	TargetPath string    `json:"target_path"`
	Total      int64     `json:"total"`
	Done       int64     `json:"done"`
	Files      int       `json:"files"`
	FilesDone  int       `json:"files_done"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Task 一次传输任务。计数用原子量;终态字段仅由协调 goroutine 写、快照读加锁。
type Task struct {
	ID         string
	Source     Endpoint
	Target     Endpoint
	SourceName string
	TargetName string
	CreatedAt  time.Time

	total    atomic.Int64
	done     atomic.Int64
	files    atomic.Int64
	fdone    atomic.Int64
	canceled atomic.Bool

	mu     sync.Mutex
	status string
	errStr string
}

func (t *Task) finish(status, errStr string) {
	t.mu.Lock()
	t.status, t.errStr = status, errStr
	t.mu.Unlock()
}

func (t *Task) setStatus(status string) { t.finish(status, "") }

func (t *Task) setFailed(err error) { t.finish(StatusFailed, err.Error()) }

func (t *Task) isRunning() bool { return t.Snapshot().Status == StatusRunning }

// Snapshot 生成一致的快照视图。
func (t *Task) Snapshot() TaskView {
	t.mu.Lock()
	status, errStr := t.status, t.errStr
	t.mu.Unlock()
	v := TaskView{
		ID:         t.ID,
		SourceName: t.SourceName,
		TargetName: t.TargetName,
		SourceKind: t.Source.Kind,
		TargetKind: t.Target.Kind,
		SourcePath: t.Source.Path,
		TargetPath: t.Target.Path,
		Total:      t.total.Load(),
		Done:       t.done.Load(),
		Files:      int(t.files.Load()),
		FilesDone:  int(t.fdone.Load()),
		Status:     status,
		Error:      errStr,
		CreatedAt:  t.CreatedAt,
	}
	if t.Target.Kind == "session" {
		v.TargetSID = t.Target.SID
	}
	return v
}

// SessionResolver 按会话 ID 提供远程文件系统访问;由 handler 注入,
// 避免 transfer 包反向依赖 sshmgr。
type SessionResolver func(sid string) (Side, string, error)

// Manager 传输任务管理器:统一 本机↔远程 / 远程↔远程 传输的异步任务模型。
// 任务状态仅存内存,进程重启即丢(内网工具,可接受)。
type Manager struct {
	resolver SessionResolver

	mu    sync.Mutex
	tasks map[string]*Task
	order []string // 创建顺序,稳定排序与淘汰用

	sem    chan struct{} // 全局并发拷贝信号量
	wg     sync.WaitGroup
	closed atomic.Bool
}

func NewManager(resolver SessionResolver) *Manager {
	return &Manager{
		resolver: resolver,
		tasks:    map[string]*Task{},
		sem:      make(chan struct{}, globalWorkers),
	}
}

// Create 校验并创建传输任务,异步执行。id 由调用方生成(store.NewID)。
// 创建时同步解析两端会话并 Stat 源路径,参数类错误在返回前暴露。
func (m *Manager) Create(id string, src, dst Endpoint, recursive bool) (TaskView, error) {
	if m.closed.Load() {
		return TaskView{}, fmt.Errorf("服务正在关闭,暂不接受新任务")
	}
	srcSide, srcName, err := m.resolve(src)
	if err != nil {
		return TaskView{}, fmt.Errorf("源: %w", err)
	}
	dstSide, dstName, err := m.resolve(dst)
	if err != nil {
		return TaskView{}, fmt.Errorf("目标: %w", err)
	}
	srcInfo, err := srcSide.Stat(src.Path)
	if err != nil {
		return TaskView{}, fmt.Errorf("源路径不可访问: %s: %v", src.Path, err)
	}
	if srcInfo.IsDir() && !recursive {
		return TaskView{}, fmt.Errorf("目录传输需递归模式")
	}

	task := &Task{
		ID:         id,
		Source:     src,
		Target:     dst,
		SourceName: srcName,
		TargetName: dstName,
		CreatedAt:  time.Now(),
	}
	task.setStatus(StatusRunning)
	m.add(task)
	m.wg.Add(1)
	go m.run(task, srcSide, dstSide, srcInfo.IsDir())
	return task.Snapshot(), nil
}

func (m *Manager) resolve(e Endpoint) (Side, string, error) {
	switch e.Kind {
	case "local":
		return NewLocalSide(), "本机", nil
	case "session":
		if e.SID == "" {
			return nil, "", fmt.Errorf("会话端点缺少 sid")
		}
		return m.resolver(e.SID)
	default:
		return nil, "", fmt.Errorf("端点 kind 必须是 session 或 local")
	}
}

func (m *Manager) add(t *Task) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[t.ID] = t
	m.order = append(m.order, t.ID)
	// 超出上限时淘汰一个最早的已结束任务,防止记录无限增长
	if len(m.order) > maxKeptTasks {
		for i, id := range m.order {
			t := m.tasks[id]
			if t == nil {
				continue
			}
			if st := t.Snapshot().Status; st != StatusRunning {
				delete(m.tasks, id)
				m.order = append(m.order[:i], m.order[i+1:]...)
				break
			}
		}
	}
}

// List 全部任务快照,最新创建的在前。
func (m *Manager) List() []TaskView {
	m.mu.Lock()
	tasks := make([]*Task, 0, len(m.order))
	for _, id := range m.order {
		tasks = append(tasks, m.tasks[id])
	}
	m.mu.Unlock()
	out := make([]TaskView, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.Snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Get 返回任务快照。
func (m *Manager) Get(id string) (TaskView, bool) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	m.mu.Unlock()
	if !ok {
		return TaskView{}, false
	}
	return t.Snapshot(), true
}

// Cancel 取消任务:置取消标志并立即更新展示状态,拷贝循环在下一个检查点退出。
func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	t, ok := m.tasks[id]
	m.mu.Unlock()
	if !ok {
		return false
	}
	t.canceled.Store(true)
	if t.isRunning() {
		t.setStatus(StatusCanceled)
	}
	return true
}

// Remove 取消(若在运行)并移除任务记录。
func (m *Manager) Remove(id string) bool {
	if !m.Cancel(id) {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[id]; !ok {
		return false
	}
	delete(m.tasks, id)
	for i, v := range m.order {
		if v == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return true
}

// CancelAll 取消全部运行中任务并拒绝新任务(优雅退出时调用)。
func (m *Manager) CancelAll() {
	m.closed.Store(true)
	m.mu.Lock()
	tasks := make([]*Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		tasks = append(tasks, t)
	}
	m.mu.Unlock()
	for _, t := range tasks {
		if t.isRunning() {
			t.canceled.Store(true)
			t.setStatus(StatusCanceled)
		}
	}
}

// WaitIdle 等待全部任务收尾,超时返回 false。
func (m *Manager) WaitIdle(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// ---------- 执行 ----------

type planItem struct {
	src, dst string
	size     int64
}

// run 任务协调:目录先枚举计划清单并建目录树,再由并发 worker 拷贝文件。
func (m *Manager) run(task *Task, src, dst Side, dir bool) {
	defer m.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			task.setFailed(fmt.Errorf("传输异常: %v", r))
		}
	}()

	if !dir {
		fi, err := src.Stat(task.Source.Path)
		if err != nil {
			task.setFailed(fmt.Errorf("源路径不可访问: %s: %v", task.Source.Path, err))
			return
		}
		task.total.Store(fi.Size())
		task.files.Store(1)
		err = m.copyOne(task, src, dst, planItem{src: task.Source.Path, dst: task.Target.Path, size: fi.Size()})
		switch {
		case err != nil && !errors.Is(err, errCanceled):
			task.setFailed(err)
		case task.canceled.Load():
			task.setStatus(StatusCanceled)
		default:
			task.setStatus(StatusDone)
		}
		return
	}
	m.runDir(task, src, dst)
}

func (m *Manager) runDir(task *Task, src, dst Side) {
	dirs, files, total, err := buildPlan(src, dst, task.Source.Path, task.Target.Path)
	if err != nil {
		task.setFailed(fmt.Errorf("遍历源目录失败: %s: %v", task.Source.Path, err))
		return
	}
	task.total.Store(total)
	task.files.Store(int64(len(files)))

	// 建目录树(含目标根)
	if err := dst.MkdirAll(task.Target.Path); err != nil {
		task.setFailed(fmt.Errorf("创建目标目录失败: %s: %v", task.Target.Path, err))
		return
	}
	for _, d := range dirs {
		if task.canceled.Load() {
			task.setStatus(StatusCanceled)
			return
		}
		if err := dst.MkdirAll(d.dst); err != nil {
			task.setFailed(fmt.Errorf("创建目录失败: %s: %v", d.dst, err))
			return
		}
	}

	// 并发 worker 拷贝;首个错误置取消标志实现快速失败
	queue := make(chan planItem)
	var workers sync.WaitGroup
	var errOnce sync.Once
	var runErr error
	for i := 0; i < workersPerTask; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for item := range queue {
				if task.canceled.Load() {
					continue // 排空队列
				}
				if err := m.copyOne(task, src, dst, item); err != nil {
					if errors.Is(err, errCanceled) {
						continue
					}
					errOnce.Do(func() {
						runErr = err
						task.canceled.Store(true)
					})
				}
			}
		}()
	}
	for _, f := range files {
		if task.canceled.Load() {
			break
		}
		queue <- f
	}
	close(queue)
	workers.Wait()

	switch {
	case runErr != nil:
		task.setFailed(runErr)
	case task.canceled.Load():
		task.setStatus(StatusCanceled)
	default:
		task.setStatus(StatusDone)
	}
}

// buildPlan 遍历源目录,返回目录/文件清单与总字节数。
// 源侧路径用 src 端点约定拼接,目标侧路径用 dst 端点约定拼接。
func buildPlan(src, dst Side, srcRoot, dstRoot string) (dirs, files []planItem, total int64, err error) {
	var walk func(sp, dp string) error
	walk = func(sp, dp string) error {
		infos, err := src.ReadDir(sp)
		if err != nil {
			return err
		}
		for _, fi := range infos {
			if fi.IsDir() {
				sp2 := src.Join(sp, fi.Name())
				dp2 := dst.Join(dp, fi.Name())
				dirs = append(dirs, planItem{src: sp2, dst: dp2})
				if err := walk(sp2, dp2); err != nil {
					return err
				}
			} else {
				files = append(files, planItem{
					src:  src.Join(sp, fi.Name()),
					dst:  dst.Join(dp, fi.Name()),
					size: fi.Size(),
				})
				total += fi.Size()
			}
		}
		return nil
	}
	err = walk(srcRoot, dstRoot)
	return dirs, files, total, err
}

// copyOne 经全局信号量拷贝单文件(1MB 缓冲,逐块检查取消并累计进度)。
// 阻塞中的单个 SFTP 读写无法被中断,取消在下一个数据块边界生效。
func (m *Manager) copyOne(task *Task, src, dst Side, item planItem) (err error) {
	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	parent := dst.Dir(item.dst)
	if err := dst.MkdirAll(parent); err != nil {
		return fmt.Errorf("创建目录失败: %s: %v", parent, err)
	}
	srcf, err := src.Open(item.src)
	if err != nil {
		return fmt.Errorf("打开失败: %s: %v", item.src, err)
	}
	defer srcf.Close()
	dstf, err := dst.Create(item.dst)
	if err != nil {
		return fmt.Errorf("创建失败: %s: %v", item.dst, err)
	}
	defer func() {
		if cerr := dstf.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("写入收尾失败: %s: %v", item.dst, cerr)
		}
	}()

	buf := make([]byte, copyBufSize)
	for {
		if task.canceled.Load() {
			return errCanceled
		}
		n, rerr := srcf.Read(buf)
		if n > 0 {
			if _, werr := dstf.Write(buf[:n]); werr != nil {
				return fmt.Errorf("写入失败: %s: %v", item.dst, werr)
			}
			task.done.Add(int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("读取失败: %s: %v", item.src, rerr)
		}
	}
	task.fdone.Add(1)
	return nil
}
