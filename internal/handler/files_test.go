package handler

import (
	"io"
	"strings"
	"testing"

	"webssh/internal/sshmgr"
)

func TestValidName(t *testing.T) {
	ok := []string{"a", "中文目录", "a.b-c_d", "  spaced  "}
	for _, name := range ok {
		if err := validName(name); err != nil {
			t.Fatalf("validName(%q) 不应报错: %v", name, err)
		}
	}
	bad := []string{
		"", "   ", // 空
		strings.Repeat("a", 65), // 超 64 字符
		"a/b", "a\\b",           // 路径分隔符
		".", "..", // 相对目录
		"a\x01b", // 控制字符
	}
	for _, name := range bad {
		if err := validName(name); err == nil {
			t.Fatalf("validName(%q) 应报错", name)
		}
	}
}

func TestResolveRemotePath(t *testing.T) {
	// 仅覆盖不依赖远程 Home() 的分支("~" 解析需要 SSH 连接,归入集成测试)
	s := &sshmgr.Session{}
	cases := []struct {
		in   string
		want string
	}{
		{"/abs/path", "/abs/path"},
		{"/a//b/./c/..", "/a/b"},
		{"rel/path", "rel/path"},
	}
	for _, tc := range cases {
		got, err := resolveRemotePath(s, tc.in)
		if err != nil {
			t.Fatalf("resolveRemotePath(%q) 报错: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("resolveRemotePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestScanOSC7(t *testing.T) {
	// BEL 结尾
	s := &sshmgr.Session{}
	sshmgr.ScanOSC7(s, []byte("hello\x1b]7;file://myhost/var/log\x07rest"))
	if got := s.Cwd(); got != "/var/log" {
		t.Fatalf("Cwd = %q, want /var/log", got)
	}

	// ST(ESC \)结尾 + URL 编码路径
	s2 := &sshmgr.Session{}
	sshmgr.ScanOSC7(s2, []byte("\x1b]7;file://h/my%20dir\x1b\\"))
	if got := s2.Cwd(); got != "/my dir" {
		t.Fatalf("Cwd = %q, want /my dir", got)
	}

	// 单块多个序列
	s3 := &sshmgr.Session{}
	sshmgr.ScanOSC7(s3, []byte("\x1b]7;file://h/a\x07mid\x1b]7;file://h/b\x07"))
	if got := s3.Cwd(); got != "/b" {
		t.Fatalf("Cwd = %q, want /b(最后一个序列生效)", got)
	}

	// 序列被截断(无结束符)不更新
	s4 := &sshmgr.Session{}
	sshmgr.ScanOSC7(s4, []byte("\x1b]7;file://h/tmp"))
	if got := s4.Cwd(); got != "" {
		t.Fatalf("截断序列不应更新 Cwd,得到 %q", got)
	}

	// 空主机名(file:///path)仍可解析
	s5 := &sshmgr.Session{}
	sshmgr.ScanOSC7(s5, []byte("\x1b]7;file:///etc\x07"))
	if got := s5.Cwd(); got != "/etc" {
		t.Fatalf("Cwd = %q, want /etc", got)
	}
}

func TestOsc7ReaderSplitSequence(t *testing.T) {
	s := &sshmgr.Session{}
	in := "abc\x1b]7;file://h/tmp\x07def"
	r := &osc7Reader{r: strings.NewReader(in), s: s}

	// 2 字节小缓冲强制序列跨块切分,验证 carry 逻辑
	var out []byte
	buf := make([]byte, 2)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if string(out) != in {
		t.Fatalf("osc7Reader 应原样透传,得到 %q", out)
	}
	if got := s.Cwd(); got != "/tmp" {
		t.Fatalf("Cwd = %q, want /tmp", got)
	}
}

func TestAtoiDefault(t *testing.T) {
	cases := []struct {
		in   string
		def  int
		want int
	}{
		{"", 80, 80},
		{"120", 80, 120},
		{"abc", 80, 80},
		{"-1", 80, -1},
	}
	for _, tc := range cases {
		if got := atoiDefault(tc.in, tc.def); got != tc.want {
			t.Fatalf("atoiDefault(%q, %d) = %d, want %d", tc.in, tc.def, got, tc.want)
		}
	}
}
