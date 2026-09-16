//go:build integration

package sshmgr

import (
	"strings"
	"testing"
)

// TestIntegrationAddK8sForwardPreflight 验证 k8s 转发的添加预检在
// 无 kubectl 环境(testssh 在本机运行,通常没有集群)下会明确报错,
// 而不是"监听成功但不可用"的假成功。
func TestIntegrationAddK8sForwardPreflight(t *testing.T) {
	s := openTestSession(t, New(t.TempDir(), 8))

	// 参数校验先行:非法 namespace 不发起任何远端探测
	if _, err := s.AddForward("k8s", "", "BAD NS", "svc/x", "80", 18080, 0); err == nil ||
		!strings.Contains(err.Error(), "namespace") {
		t.Fatalf("非法 namespace 应报校验错误,实际: %v", err)
	}

	// 合法格式但不存在的资源:预检应以 kubectl 相关错误拒绝
	_, err := s.AddForward("k8s", "", "default", "svc/webssh-definitely-not-here", "80", 18081, 0)
	if err == nil {
		t.Fatal("预检不应通过(本机无该服务)")
	}
	if !strings.Contains(err.Error(), "kubectl") && !strings.Contains(err.Error(), "预检") {
		t.Fatalf("错误应指向 kubectl 预检,实际: %v", err)
	}
	// 预检失败不得留下监听端口
	if f := s.ListForwards(); len(f) != 0 {
		t.Fatalf("预检失败后不应登记转发: %+v", f)
	}
}
