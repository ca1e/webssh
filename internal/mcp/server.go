// Package webssh MCP 服务:把现有 SSH 管理能力封装为工具集。
// 核心不变式:Agent 永不接触密码/私钥。ssh_exec 接受任意命令字符串,
// 超时/输出上限/下载上限由 mcp.json 控制。
//
// 装配:mcp.NewServer + 官方 go-sdk Streamable HTTP 挂 gin /mcp;
// 认证:预共享口令 Bearer(ConstantTimeCompare);未配置口令 = MCP 整体
// 禁用(fail-closed,/mcp 404 而非 401,不探测不暴露服务存在性)。
package mcp

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"webssh/internal/auth"
	"webssh/internal/k8smgr"
	"webssh/internal/mcp/pathguard"
	"webssh/internal/sshmgr"
	"webssh/internal/store"
)

// version 随实现发布,变更工具语义时递增。
const version = "1.0.0"

// Server MCP 装配体:配置/模板/路径白名单的持有者,工具注册入口。
type Server struct {
	cfgDir string
	st     *store.Store
	mgr    *sshmgr.Manager
	k8s    *k8smgr.Manager
	auth   *auth.Store // 锁屏密码:未显式配置 token 时的 MCP 认证回落

	mu     sync.RWMutex
	conf   *Config
	guard  *pathguard.Guard
	mcpSrv *mcp.Server

	// envEnabled/envToken 环境变量在构造时固定(容器注入点)。
	envEnabled bool
	envToken   string
}

// New 加载配置并构建 MCP Server;配置非法返回错误(启动 fatal)。
func New(cfgDir string, st *store.Store, mgr *sshmgr.Manager, authSt *auth.Store) (*Server, error) {
	s := &Server{
		cfgDir: cfgDir,
		st:     st,
		mgr:    mgr,
		k8s:    k8smgr.New(),
		auth:   authSt,
	}
	s.envToken = os.Getenv("WEBSSH_MCP_TOKEN")
	s.envEnabled = os.Getenv("WEBSSH_MCP_ENABLED") != "false"
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load (重)加载配置并重建整棵工具注册树;调用方须在持锁外调用,
// 失败时 Server 保持旧状态(Reload 语义)。
func (s *Server) load() error {
	cfg, err := Load(s.cfgDir)
	if err != nil {
		return err
	}
	guard, err := pathguard.New(cfg.Defaults.PathWhitelist.Enabled, cfg.Defaults.PathWhitelist.Paths)
	if err != nil {
		return err
	}
	srv := s.buildTools(cfg, guard)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conf, s.guard, s.mcpSrv = cfg, guard, srv
	return nil
}

// Reload SIGHUP 热重载:失败保持旧策略并返回错误(调用方打告警)。
func (s *Server) Reload() error {
	return s.load()
}

// tokenSource 认证口令来源(env 显式指定 / 配置文件 / 锁屏密码回落)。
type tokenSource int

const (
	tokenNone       tokenSource = iota // 无任何口令:MCP 整体禁用(fail-closed)
	tokenShared                        // WEBSSH_MCP_TOKEN 或 mcp.json.token,恒时比较
	tokenLockscreen                    // 锁屏密码(auth.json bcrypt),逐请求 bcrypt 校验
)

// tokenSourceKind 当前生效的口令来源(env 优先,其次 mcp.json,最后回落锁屏密码)。
func (s *Server) tokenSourceKind() tokenSource {
	if s.envToken != "" {
		return tokenShared
	}
	s.mu.RLock()
	hasFileToken := s.conf.Token != ""
	s.mu.RUnlock()
	if hasFileToken {
		return tokenShared
	}
	if s.auth != nil && s.auth.HasPassword() {
		return tokenLockscreen
	}
	return tokenNone
}

// tokenValue 生效口令:环境变量优先(容器注入),其次 mcp.json;两者皆缺省时
// 复用锁屏密码(返回空串——锁屏路径走 bcrypt 比对,不做明文恒时比较)。
func (s *Server) tokenValue() string {
	if s.envToken != "" {
		return s.envToken
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conf.Token
}

// Enabled MCP 是否对外可用:任何口令来源都不存在 = 整体禁用(fail-closed)。
func (s *Server) Enabled() bool {
	return s.envEnabled && s.tokenSourceKind() != tokenNone
}

// TokenSourceName 当前口令来源的可读名称(启动日志用)。
func (s *Server) TokenSourceName() string {
	if !s.envEnabled {
		return "已禁用"
	}
	switch s.tokenSourceKind() {
	case tokenShared:
		if s.envToken != "" {
			return "WEBSSH_MCP_TOKEN"
		}
		return "mcp.json"
	case tokenLockscreen:
		return "锁屏密码"
	default:
		return "无(未配置口令,/mcp 返回 404)"
	}
}

// currentServer 返回当前已注册工具的 mcp.Server(热重载原子切换)。
func (s *Server) currentServer() *mcp.Server {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mcpSrv
}

// Register 把 /mcp 挂到 gin 路由(独立 Bearer 中间件,不走 cookie 解锁)。
// 路由恒注册;禁用态与"口令来源逐请求评估"都在 bearerMiddleware 内
// fail-closed(404),锁屏密码后设/更换无需重启。
func (s *Server) Register(r gin.IRouter) {
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.currentServer() },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	r.Any("/mcp", s.bearerMiddleware(), gin.WrapH(handler))
}

// bearerMiddleware 预共享口令校验:Authorization: Bearer <口令>。显式配置的
// token 做 ConstantTimeCompare;回落锁屏密码时走 bcrypt(比对本身即安全比较,
// 单次约几十 ms)。不匹配 → 401 文本(不区分缺失与错误,避免探测);
// MCP 禁用 → 404(fail-closed)。
func (s *Server) bearerMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		kind := s.tokenSourceKind()
		if !s.envEnabled || kind == tokenNone {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		got := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		ok := false
		if kind == tokenLockscreen {
			ok = s.auth.MatchesPassword(got)
		} else {
			ok = subtle.ConstantTimeCompare([]byte(got), []byte(s.tokenValue())) == 1
		}
		if !ok {
			c.Header("Content-Type", "text/plain; charset=utf-8")
			c.String(http.StatusUnauthorized, "令牌无效或缺失;请在客户端配置 Authorization: Bearer <token>")
			c.Abort()
			return
		}
		c.Next()
	}
}

// callLog 每工具调用一行留痕:工具/主机/结果/耗时(时间戳由 log 包自带)。
// 无独立审计组件;将来治理需求回归时在此挂接。
func callLog(tool, host string, err error, start time.Time) {
	result := "ok"
	if err != nil {
		result = "deny: " + err.Error()
	}
	log.Printf("[mcp] tool=%s host=%s result=%s elapsed=%s", tool, host, result, time.Since(start).Round(time.Millisecond))
}

// ---------- 工具公共前置 ----------

// resolveHost 按名称或 ID 解析主机;不存在给可纠偏的友好错误。
func (s *Server) resolveHost(nameOrID string) (store.Host, error) {
	if strings.TrimSpace(nameOrID) == "" {
		return store.Host{}, errors.New("缺少 host 参数;请传入主机名称或 ID,可先用 ssh_list_hosts 查询")
	}
	hosts := s.st.List()
	for _, h := range hosts {
		if h.ID == nameOrID {
			if !s.hostVisible(h) {
				break // 不可见主机按不存在处理,不泄露存在性
			}
			return h, nil
		}
	}
	for _, h := range hosts {
		if h.Name == nameOrID {
			if !s.hostVisible(h) {
				break
			}
			return h, nil
		}
	}
	return store.Host{}, fmt.Errorf("主机 %q 不存在;请用 ssh_list_hosts 查看可用主机,或确认名称拼写", nameOrID)
}

// rejectTelnet MCP 对 telnet 主机统一拒绝:仅 SSH 主机支持 MCP 操作。
func rejectTelnet(h store.Host) error {
	if store.NormalizeConnType(h.ConnType) == store.ConnTelnet {
		return errors.New("仅 SSH 主机支持 MCP 操作")
	}
	return nil
}

// hostVisible 可见主机开关:关闭 = 全部可见;开启 = 仅白名单内主机可见。
func (s *Server) hostVisible(h store.Host) bool {
	s.mu.RLock()
	vh := s.conf.VisibleHosts
	s.mu.RUnlock()
	if !vh.Enabled {
		return true
	}
	for _, id := range vh.IDs {
		if id == h.ID {
			return true
		}
	}
	return false
}

// whitelistNote 白名单开启时追加到工具描述,让 Agent 提前知晓约束。
func whitelistNote(guard *pathguard.Guard) string {
	if !guard.Enabled() {
		return ""
	}
	return "当前已启用路径白名单,命令与文件路径仅限: " + fmt.Sprint(guard.Prefixes())
}

// buildTools 按域注册工具(域由 mcp.json 门控,全默认全开)。
func (s *Server) buildTools(cfg *Config, guard *pathguard.Guard) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "webssh", Version: version}, nil)
	allowed := func(name string) bool {
		// 工具白名单:仅名单内的工具注册;空名单 = 不暴露任何工具
		for _, n := range cfg.ToolFilter.Tools {
			if n == name {
				return true
			}
		}
		return false
	}
	if cfg.Domains["hosts"] {
		if allowed("ssh_list_hosts") {
			mcp.AddTool(srv, &mcp.Tool{
				Name:        "ssh_list_hosts",
				Description: "列出全部主机元数据(名称/IP/端口/用户/认证类型标识)。无任何凭据内容。",
			}, s.toolListHosts)
		}
		if allowed("ssh_host_info") {
			mcp.AddTool(srv, &mcp.Tool{
				Name:        "ssh_host_info",
				Description: "查询单台主机的配置详情(脱敏投影:只回认证类型标识与 Has* 布尔,无凭据)。",
			}, s.toolHostInfo)
		}
		if allowed("ssh_test_connection") {
			mcp.AddTool(srv, &mcp.Tool{
				Name:        "ssh_test_connection",
				Description: "拨号+认证+host key 校验验证主机可达性,不保留连接。",
			}, s.toolTestConnection)
		}
	}
	if cfg.Domains["exec"] {
		if allowed("ssh_exec") {
			mcp.AddTool(srv, &mcp.Tool{
				Name:        "ssh_exec",
				Description: "在指定主机上执行任意命令并返回 stdout/stderr/exit_code。超时与输出上限由 mcp.json defaults 控制。" + whitelistNote(guard),
			}, s.toolExec)
		}
	}
	if cfg.Domains["files"] {
		if allowed("ssh_download_file") {
			mcp.AddTool(srv, &mcp.Tool{
				Name:        "ssh_download_file",
				Description: "经 SFTP 下载远端文件到服务端暂存目录,返回本机路径/sha256/大小。任意可读路径(绝对路径或 ~/)。" + whitelistNote(guard),
			}, s.toolDownloadFile)
		}
	}
	if cfg.Domains["monitor"] {
		if allowed("ssh_system_info") {
			mcp.AddTool(srv, &mcp.Tool{
				Name:        "ssh_system_info",
				Description: "采集主机静态信息:主机名/系统/内核/架构/CPU 型号与核数/磁盘列表/sshd 转发配置摘要。",
			}, s.toolSystemInfo)
		}
		if allowed("ssh_cpu_memory") {
			mcp.AddTool(srv, &mcp.Tool{
				Name:        "ssh_cpu_memory",
				Description: "采样 CPU/内存/负载/交换分区(1 秒差分窗口)。",
			}, s.toolCPUMemory)
		}
		if allowed("ssh_disk_usage") {
			mcp.AddTool(srv, &mcp.Tool{
				Name:        "ssh_disk_usage",
				Description: "df 磁盘空间使用明细(排除 tmpfs 等伪文件系统)。",
			}, s.toolDiskUsage)
		}
		if allowed("ssh_network_status") {
			mcp.AddTool(srv, &mcp.Tool{
				Name:        "ssh_network_status",
				Description: "网络吞吐采样(1 秒差分)+ 接口地址列表 + 累计字节数。",
			}, s.toolNetworkStatus)
		}
		if allowed("ssh_service_status") {
			mcp.AddTool(srv, &mcp.Tool{
				Name:        "ssh_service_status",
				Description: "查询单个 systemd 单元的只读状态(ActiveState/SubState/描述/启动时间),不含任何启停操作。",
			}, s.toolServiceStatus)
		}
		if allowed("ssh_docker_status") {
			mcp.AddTool(srv, &mcp.Tool{
				Name:        "ssh_docker_status",
				Description: "Docker 可用性、容器列表与引擎/Compose 信息(只读)。",
			}, s.toolDockerStatus)
		}
		if allowed("ssh_k8s_status") {
			mcp.AddTool(srv, &mcp.Tool{
				Name:        "ssh_k8s_status",
				Description: "Kubernetes 集群探测结果(缓存,只读;不可用时给出原因)。",
			}, s.toolK8sStatus)
		}
	}
	return srv
}

// acquire 会话解析:复用该主机的活会话(Web 打开的或前次 MCP 的),
// 没有则自动 OpenSession(凭据引用 + 跳板 + TOFU 全链路);调用期间持有
// 租约防孤儿收割,结束由调用方 ReleaseLease,会话交给宽限期回收。
func (s *Server) acquire(ctx context.Context, h store.Host) (*sshmgr.Session, error) {
	for _, sess := range s.mgr.List() {
		if sess.ConnType() == store.ConnSSH && sess.Host.ID == h.ID {
			if err := sess.AcquireLease(); err == nil {
				return sess, nil
			}
		}
	}
	var jump *store.Host
	if h.JumpID != "" {
		j, ok := s.st.Get(h.JumpID)
		if !ok {
			return nil, errors.New("跳板机不存在(可能已被删除);请在 Web 端修正该主机的跳板配置")
		}
		jump = &j
	}
	// trustNewKey=false:TOFU 语义,首次连接自动记录主机密钥;密钥变更仍拒绝
	sess, err := s.mgr.OpenSession(ctx, h, jump, false)
	if err != nil {
		return nil, fmt.Errorf("连接主机 %s 失败:%v;请在 Web 端用「测试连接」验证凭据、网络与主机密钥", h.Name, err)
	}
	if err := sess.AcquireLease(); err != nil {
		sess.Close()
		return nil, fmt.Errorf("主机 %s 的会话刚建立即断开,请稍后重试", h.Name)
	}
	return sess, nil
}

// release 成对释放租约(defer 用)。
func release(sess *sshmgr.Session) {
	if sess != nil {
		sess.ReleaseLease()
	}
}
