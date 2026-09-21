package pathguard

import (
	"strings"
	"testing"
)

func TestNewRejectsRelative(t *testing.T) {
	if _, err := New(true, []string{"var/log", "/a//b"}); err == nil {
		t.Fatal("相对/未规范化前缀应拒绝")
	}
}

func TestDisabledAllowsAll(t *testing.T) {
	g, err := New(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g.Enabled() {
		t.Fatal("默认应关闭")
	}
	if _, err := g.CheckStatic("/etc/shadow", "/root"); err != nil {
		t.Fatalf("关闭时不限路径: %v", err)
	}
}

func TestCheckStaticPrefixAndExpand(t *testing.T) {
	g, _ := New(true, []string{"/var/log/", "/data"})
	home := "/root"
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"/var/log/syslog", "/var/log/syslog", true},
		{"/var/log/../log/a", "/var/log/a", true},
		{"~/x.log", "/root/x.log", false}, // ~ 展开到 /root,不在白名单 → 拒绝
		{"/data/file", "/data/file", true},
		{"/etc/passwd", "/etc/passwd", false},
		{"relative", "/relative", false},
	} {
		got, err := g.CheckStatic(c.in, home)
		if c.ok != (err == nil) {
			t.Fatalf("%s: err=%v", c.in, err)
		}
		if c.ok && got != c.want {
			t.Fatalf("%s: got %s want %s", c.in, got, c.want)
		}
	}
}

// TestCheckReadStaticFallback 无会话时只验证静态段:展开/规范化/前缀匹配。
func TestCheckReadStaticFallback(t *testing.T) {
	g, _ := New(true, []string{"/var/log/"})
	if !g.Enabled() {
		t.Fatal("应开启")
	}
	if _, err := g.CheckStatic("/var/log//sub/../a", ""); err != nil {
		t.Fatalf("白名单内应通过: %v", err)
	}
	if _, err := g.CheckStatic("/var/logs/a", ""); err == nil {
		t.Fatal("/var/logs 不应命中 /var/log/ 前缀")
	}
}

func TestPrefixesNormalized(t *testing.T) {
	g, _ := New(true, []string{"/a/b"})
	if got := g.Prefixes(); len(got) != 1 || got[0] != "/a/b/" {
		t.Fatalf("前缀应规范化带尾斜杠: %v", got)
	}
	if !strings.HasSuffix(g.Prefixes()[0], "/") {
		t.Fatal("前缀应以 / 结尾")
	}
}
