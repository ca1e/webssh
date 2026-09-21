package sshmgr

import (
	"testing"
	"time"
)

// 孤儿回收机制单测:不拨真实 SSH,手工构造 Manager/Session(client 为 nil,
// Close 已 nil-safe),用小宽限期验证 Attach/Detach 引用计数与计时器行为。

func newOrphanFixture(grace time.Duration) (*Manager, *Session) {
	m := &Manager{sessions: map[string]*Session{}, orphanGrace: grace}
	s := &Session{ID: "orphan-1", mgr: m, connType: "ssh", forwards: map[string]*Forward{}}
	m.sessions[s.ID] = s
	return m, s
}

func alive(m *Manager, id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.sessions[id]
	return ok
}

func sessionClosed(s *Session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// grace 后仍未被 attach 的会话(新建即无终端 / 终端全部断开)应被回收并从
// 管理器摘除
func TestOrphanReapedAfterGrace(t *testing.T) {
	m, s := newOrphanFixture(80 * time.Millisecond)
	s.armOrphan()
	time.Sleep(300 * time.Millisecond)
	if !sessionClosed(s) || alive(m, s.ID) {
		t.Fatalf("宽限期后应自动回收: closed=%v alive=%v", sessionClosed(s), alive(m, s.ID))
	}
}

// attach 应取消待命回收;grace 内重连(模拟 F5)会话无损存活,
// detach 后重新计时并回收
func TestAttachCancelsReap(t *testing.T) {
	m, s := newOrphanFixture(80 * time.Millisecond)
	s.armOrphan()
	if err := s.AttachTerminal(); err != nil {
		t.Fatalf("attach 失败: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if !alive(m, s.ID) {
		t.Fatal("有终端附着时不应回收")
	}
	s.DetachTerminal()
	time.Sleep(300 * time.Millisecond)
	if sessionClosed(s) != true || alive(m, s.ID) {
		t.Fatalf("detach 后宽限期应回收: closed=%v alive=%v", sessionClosed(s), alive(m, s.ID))
	}
}

// 多终端分屏:引用未归零不回收,归零后才启动计时
func TestRefCountKeepsSessionAlive(t *testing.T) {
	m, s := newOrphanFixture(80 * time.Millisecond)
	_ = s.AttachTerminal()
	_ = s.AttachTerminal()
	s.DetachTerminal()
	time.Sleep(300 * time.Millisecond)
	if !alive(m, s.ID) {
		t.Fatal("仍有 1 个终端附着时不应回收")
	}
	s.DetachTerminal()
	time.Sleep(300 * time.Millisecond)
	if alive(m, s.ID) {
		t.Fatal("引用归零并超过宽限期后应回收")
	}
}

// 已关闭会话:Attach 报错、Detach 静默、重复 Close 幂等
func TestAttachAfterCloseFails(t *testing.T) {
	m, s := newOrphanFixture(time.Hour)
	s.Close()
	if err := s.AttachTerminal(); err == nil {
		t.Fatal("closed 会话 attach 应报错")
	}
	s.DetachTerminal() // 不应 panic 或误布防
	s.Close()          // 幂等
	if alive(m, s.ID) {
		t.Fatal("Close 应移除会话")
	}
}

// orphanGrace<=0 视为关闭该机制(测试/手工构造 Manager 的默认值不会误杀会话)
func TestZeroGraceDisablesReap(t *testing.T) {
	m, s := newOrphanFixture(0)
	s.armOrphan()
	s.AttachTerminal()
	s.DetachTerminal()
	time.Sleep(200 * time.Millisecond)
	if !alive(m, s.ID) {
		t.Fatal("宽限期为 0 时不应回收任何会话")
	}
}

// MCP 调用租约:Acquire 阻止宽限期回收,Release 归零后重新计时。
func TestLeaseBlocksReap(t *testing.T) {
	m, s := newOrphanFixture(30 * time.Millisecond)
	if err := s.AcquireLease(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if !alive(m, s.ID) {
		t.Fatal("租约未归零期间会话被回收")
	}
	s.ReleaseLease()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !alive(m, s.ID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("ReleaseLease 后宽限期到仍未回收")
}

// 已关闭会话 AcquireLease 报错;Release 对关闭/越界计数静默。
func TestLeaseOnClosedSession(t *testing.T) {
	_, s := newOrphanFixture(time.Hour)
	s.Close()
	if err := s.AcquireLease(); err == nil {
		t.Fatal("closed 会话 AcquireLease 应报错")
	}
	s.ReleaseLease() // 不应 panic
}
