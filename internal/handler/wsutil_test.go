package handler

import (
	"io"
	"net/http/httptest"
	"testing"
)

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		host   string
		want   bool
	}{
		{"非浏览器客户端无 Origin", "", "127.0.0.1:8090", true},
		{"同源", "http://127.0.0.1:8090", "127.0.0.1:8090", true},
		{"同源 localhost", "http://localhost:5174", "localhost:5174", true},
		{"跨源拒绝", "http://evil.example", "127.0.0.1:8090", false},
		{"同 IP 不同端口", "http://127.0.0.1:9999", "127.0.0.1:8090", false},
		{"跨源同 host:port 不同 scheme", "https://127.0.0.1:8090", "127.0.0.1:8090", true},
		{"Origin 解析失败", "://bad", "127.0.0.1:8090", false},
		{"Origin 仅协议", "http://", "127.0.0.1:8090", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/sessions/x/ws", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if got := sameOrigin(r); got != tc.want {
				t.Fatalf("sameOrigin(origin=%q host=%q) = %v, want %v", tc.origin, tc.host, got, tc.want)
			}
		})
	}
}

// chunkReader 按预设分片依次返回数据,模拟 PTY 输出的任意切帧。
type chunkReader struct {
	chunks [][]byte
	i      int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.i >= len(c.chunks) {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[c.i])
	c.i++
	return n, nil
}

func drainStripReader(t *testing.T, r io.Reader) string {
	t.Helper()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return string(out)
}

func TestEchoStripReader(t *testing.T) {
	line := osc7HookLineForTest()
	pattern := line + "\r\n"

	cases := []struct {
		name   string
		chunks [][]byte
		want   string
	}{
		{
			name: "两次回显整体剥离",
			chunks: [][]byte{
				[]byte("banner\r\n"),
				[]byte(pattern),
				[]byte("[root@h ~]# "),
				[]byte(pattern),
				[]byte("ready\r\n"),
			},
			want: "banner\r\n[root@h ~]# ready\r\n",
		},
		{
			name: "回显跨块切分",
			chunks: [][]byte{
				[]byte("banner\r\n" + line[:50]),
				[]byte(line[50:120]),
				[]byte(line[120:] + "\r\n[root@h ~]# " + line[:7]),
				[]byte(line[7:] + "\r\n"),
				[]byte("done"),
			},
			want: "banner\r\n[root@h ~]# done",
		},
		{
			name: "尾部前缀悬置后确认放行",
			chunks: [][]byte{
				[]byte(pattern[:40]),
				[]byte(pattern[40:]), // 第一次回显完整
				[]byte(line[:10]),    // 悬置的前缀
				[]byte("xx" + pattern),
				[]byte("end"),
			},
			want: line[:10] + "xxend",
		},
		{
			name: "仅一次回显时其余照常透传",
			chunks: [][]byte{
				[]byte(pattern + "[root@h ~]# ls\r\nfile.txt\r\n"),
			},
			want: "[root@h ~]# ls\r\nfile.txt\r\n",
		},
		{
			name: "无回显时原样透传",
			chunks: [][]byte{
				[]byte("login: "),
				[]byte("password: "),
			},
			want: "login: password: ",
		},
		{
			name: "暂扣前缀遇不匹配续流时放行",
			chunks: [][]byte{
				[]byte(line + "\r"), // 是 pattern 真前缀,先暂扣
				[]byte("X\r\nrest"), // 续流打破匹配,前缀原样放出
			},
			want: line + "\rX\r\nrest",
		},
	}
	for _, tc := range cases {
		got := drainStripReader(t, newEchoStripReader(&chunkReader{chunks: tc.chunks}, line))
		if got != tc.want {
			t.Errorf("%s: 输出 = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestEchoStripReaderRealPayload 用与 NewTerminal 完全一致的注入行验证剥离,
// 并保证剥离不破坏紧随其后的 OSC 7 序列(cwd 跟踪依赖它)。
func TestEchoStripReaderRealPayload(t *testing.T) {
	line := osc7HookLineForTest()
	pattern := line + "\r\n"
	osc7 := "\x1b]7;file://h1/root\x1b\\"
	chunks := [][]byte{
		[]byte(pattern),
		[]byte("[root@server113 ~]# " + pattern + osc7),
	}
	r := newEchoStripReader(&chunkReader{chunks: chunks}, line)
	got := drainStripReader(t, r)
	want := "[root@server113 ~]# " + osc7
	if got != want {
		t.Errorf("输出 = %q, want %q", got, want)
	}
}

func osc7HookLineForTest() string {
	// 与 sshmgr.osc7HookLine 同源(小写跨包不可见,此处以 InitLine 语义构造)
	return "__ws_cwd(){ printf '\\033]7;file://%s%s\\033\\\\' \"${HOSTNAME:-$HOST}\" \"$PWD\"; }; " +
		"if [ -n \"${BASH_VERSION:-}\" ]; then PROMPT_COMMAND=\"__ws_cwd${PROMPT_COMMAND:+;$PROMPT_COMMAND}\"; export PROMPT_COMMAND; " +
		"elif [ -n \"${ZSH_VERSION:-}\" ]; then precmd() { __ws_cwd; }; fi"
}
