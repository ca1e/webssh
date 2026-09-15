package handler

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"webssh/internal/sshmgr"
)

// sameOrigin 校验 WebSocket 握手的 Origin 与请求 Host 同源(仅比对 host:port,
// 不比对 scheme:反向代理 TLS 终结后 r 自身是 http,以 scheme 判定会误拒)。
// Origin 为空(非浏览器客户端,如 curl)时放行;浏览器发起的跨源升级一律拒绝,
// 防御恶意网页借用户浏览器对 127.0.0.1 服务发起跨站 WebSocket 劫持。
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host != "" && u.Host == r.Host
}

// newUpgrader 创建统一配置的 WebSocket Upgrader(强制同源校验)。
// 全部 WS 入口共用本工厂,禁止各自定义 CheckOrigin。
func newUpgrader(readSize, writeSize int) websocket.Upgrader {
	return websocket.Upgrader{
		ReadBufferSize:  readSize,
		WriteBufferSize: writeSize,
		CheckOrigin:     sameOrigin,
	}
}

// wsWriteDeadline 单帧写入超时:客户端停止读取时避免写侧永久阻塞。
const wsWriteDeadline = 10 * time.Second

// wsBridge 封装 WebSocket 双向泵送的公共逻辑:stdout→二进制帧、WS 消息→回调、
// 心跳与单次关闭协调,替代各 WS handler 手写的读写泵(此前四处各写一份)。
type wsBridge struct {
	ws   *websocket.Conn
	wmu  sync.Mutex
	done chan struct{}
	once sync.Once
}

func newWSBridge(ws *websocket.Conn) *wsBridge {
	return &wsBridge{ws: ws, done: make(chan struct{})}
}

// close 幂等关闭:触发 done 并断开底层连接,唤醒读写两侧的 goroutine。
func (b *wsBridge) close() {
	b.once.Do(func() {
		close(b.done)
		_ = b.ws.Close()
	})
}

// wait 阻塞直至桥关闭(远端流结束或任一侧断开)。
func (b *wsBridge) wait() { <-b.done }

// doneChan 返回关闭信号 channel(供 select 监听)。
func (b *wsBridge) doneChan() <-chan struct{} { return b.done }

func (b *wsBridge) write(mt int, data []byte) error {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_ = b.ws.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
	return b.ws.WriteMessage(mt, data)
}

// pumpStdout 将 r 的输出泵送为 WS 二进制帧(异步);读到 EOF/错误时回调 onEOF
// 后关闭桥。
func (b *wsBridge) pumpStdout(r io.Reader, onEOF func()) {
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if b.write(websocket.BinaryMessage, buf[:n]) != nil {
					b.close()
					return
				}
			}
			if err != nil {
				if onEOF != nil {
					onEOF()
				}
				b.close()
				return
			}
		}
	}()
}

// pumpPing 定期发送心跳,失败即关闭桥,防止中间设备静默掐断空闲连接。
func (b *wsBridge) pumpPing(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-b.done:
			return
		case <-t.C:
			if b.write(websocket.PingMessage, nil) != nil {
				b.close()
				return
			}
		}
	}
}

// readLoop 阻塞读取 WS 消息并交给 onMessage;回调返回 false 或读错误时关闭桥。
func (b *wsBridge) readLoop(onMessage func(mt int, data []byte) bool) {
	for {
		mt, data, err := b.ws.ReadMessage()
		if err != nil {
			b.close()
			return
		}
		if onMessage != nil && !onMessage(mt, data) {
			b.close()
			return
		}
	}
}

// osc7Reader 包装 PTY 输出流:转发字节的同时扫描 OSC 7 序列,跟踪远程 shell 目录;
// 首次发现序列时调用 markOSC7(可空,解除无感注入的降级看门狗)。
type osc7Reader struct {
	r        io.Reader
	s        *sshmgr.Session
	markOSC7 func()
	sawOSC7  bool
	carry    []byte
}

func (o *osc7Reader) Read(p []byte) (int, error) {
	n, err := o.r.Read(p)
	if n > 0 {
		merged := append(o.carry, p[:n]...)
		if sshmgr.ScanOSC7(o.s, merged) {
			o.sawOSC7 = true
			if o.markOSC7 != nil {
				o.markOSC7()
			}
		}
		// 仅保留尾部:既覆盖跨帧被截断的序列(URL 编码的深路径序列可达数百字节),
		// 又限制缓冲大小
		if len(merged) > 512 {
			merged = merged[len(merged)-512:]
		}
		o.carry = append(o.carry[:0], merged...)
	}
	return n, err
}

// echoStripReader 从 PTY 输出流中剥离启动注入行的回显(echoStripReader 行为约定:
// 只按精确字节匹配,任何不匹配立即整体放弃并原样透传,绝不吞输出)。
//
// NewTerminal 向 shell 写入 osc7HookLine 后,该行会被回显两次:bash 就绪前的
// 内核 ECHO(逐字节原样,\r 经 ICRNL+ONLCR 输出为 \r\n),以及 bash/zsh 行编辑器
// 读取预输入行时的行编辑回显(TERM 具备自动换行能力时同样逐字节原样,实测
// bash 5.x + xterm-256color 命中)。两次回显均为「注入行 + \r\n」,最多剥离
// 2 次;远端为其他 shell 或 terminfo 异常导致回显形态不同时放弃,退回可见回显。
type echoStripReader struct {
	r       io.Reader
	pattern []byte
	remain  int
	scratch []byte // 底层读入暂存
	out     []byte // 处理后待交付的字节
	hold    []byte // 疑似 pattern 前缀的尾部,待下一轮拼接确认
	done    bool
}

func newEchoStripReader(r io.Reader, line string) *echoStripReader {
	return &echoStripReader{
		r:       r,
		pattern: []byte(line + "\r\n"),
		remain:  2,
		scratch: make([]byte, 32*1024),
	}
}

func (o *echoStripReader) Read(p []byte) (int, error) {
	for len(o.out) == 0 {
		if o.done {
			return o.r.Read(p)
		}
		n, err := o.r.Read(o.scratch)
		if n > 0 {
			o.out = append(o.out, o.hold...)
			o.hold = o.hold[:0]
			o.out = append(o.out, o.scratch[:n]...)
			o.strip()
		}
		if err != nil {
			// 底层结束:暂扣的尾部全部放行,后续转直通
			o.done = true
			o.out = append(o.out, o.hold...)
			o.hold = o.hold[:0]
		}
	}
	m := copy(p, o.out)
	o.out = append(o.out[:0], o.out[m:]...)
	return m, nil
}

func (o *echoStripReader) strip() {
	for !o.done {
		i := bytes.Index(o.out, o.pattern)
		if i < 0 {
			break
		}
		o.out = append(o.out[:i], o.out[i+len(o.pattern):]...)
		o.remain--
		if o.remain == 0 {
			o.done = true
		}
	}
	if o.done {
		return
	}
	// 尾部若恰好是 pattern 的前缀,可能跨读拼接成完整回显,暂扣不发;
	// 其余字节全部放行,避免长匹配窗口拖住正常输出。
	if keep := suffixPrefixLen(o.out, o.pattern); keep > 0 {
		o.hold = append(o.hold, o.out[len(o.out)-keep:]...)
		o.out = o.out[:len(o.out)-keep]
	}
}

// suffixPrefixLen 返回 buf 的最长后缀中同时是 pat 前缀的长度(不含 pat 全长)。
func suffixPrefixLen(buf, pat []byte) int {
	max := len(pat) - 1
	if len(buf) < max {
		max = len(buf)
	}
	for k := max; k > 0; k-- {
		if bytes.Equal(buf[len(buf)-k:], pat[:k]) {
			return k
		}
	}
	return 0
}
