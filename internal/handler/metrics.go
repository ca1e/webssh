package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// ---------- 主机信息采集(静态,一次性) ----------

// hostInfoScript 采集主机静态信息,输出 ===KEY=== 分段。各命令均带回退分支,
// 兼容 BusyBox/无 GNU 工具的环境:df 无 -T、ps 无 --sort 时退化为无类型/无排序输出,
// 解析端(parseDisks/parseProcs)对两种列数均容忍。
const hostInfoScript = `echo "===HOSTNAME==="
hostname -f 2>/dev/null || hostname
echo "===OS==="
(. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME") || uname -sr
echo "===KERNEL==="
uname -r
echo "===ARCH==="
uname -m
echo "===CPUMODEL==="
grep -m1 "model name" /proc/cpuinfo | cut -d: -f2
echo "===CPUCORES==="
nproc 2>/dev/null || grep -c ^processor /proc/cpuinfo
echo "===DISK==="
{ LC_ALL=C df -hT 2>/dev/null || LC_ALL=C df -h 2>/dev/null; } | grep -vE 'tmpfs|devtmpfs|overlay'
echo "===SSHCONFIG==="
# 优先 sshd -T(生效配置,含 Match 块合并结果),需 root
if sshd -T 2>/dev/null | grep -iE '^(allowtcpforwarding|disableforwarding|permitopen|allowagentforwarding)\b'; then
  echo "SOURCE=sshd -T"
else
  echo "SOURCE=config-file"
  grep -iE '^\s*(allowtcpforwarding|disableforwarding|permitopen|allowagentforwarding)\b' /etc/ssh/sshd_config /etc/ssh/sshd_config.d/* 2>/dev/null | sed 's/#.*//'
fi
echo "===TOPPROC==="
{ ps aux --sort=-%cpu 2>/dev/null || ps aux 2>/dev/null || ps w; } | head -21`

type diskInfo struct {
	Device string  `json:"device"`
	FsType string  `json:"fs_type"`
	Size   string  `json:"size"`
	Used   string  `json:"used"`
	Avail  string  `json:"avail"`
	UsePct string  `json:"use_pct"`
	Mount  string  `json:"mount"`
	UseNum float64 `json:"use_num"`
}

type procInfo struct {
	User    string  `json:"user"`
	PID     string  `json:"pid"`
	CPUPct  float64 `json:"cpu_pct"`
	MemPct  float64 `json:"mem_pct"`
	Command string  `json:"command"`
}

// sshConfigInfo 汇总与转发能力直接相关的 sshd 配置项。
type sshConfigInfo struct {
	AllowTcpForwarding string `json:"allow_tcp_forwarding"` // yes/no/local/remote，空串=未知
	DisableForwarding  string `json:"disable_forwarding"`   // yes/no
	PermitOpen         string `json:"permit_open"`          // any 或 host:port 列表
	AllowAgentForward  string `json:"allow_agent_forwarding"`
	Source             string `json:"source"` // "sshd -T" | "config-file" | ""
}

func (a *API) hostInfo(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	out, err := s.Run(hostInfoScript)
	if err != nil && out == "" {
		fail(c, 400, "采集主机信息失败: "+err.Error())
		return
	}
	sections := parseSections(out)

	disks := parseDisks(sections["DISK"])
	procs := parseProcs(sections["TOPPROC"])

	cores, _ := strconv.Atoi(strings.TrimSpace(sections["CPUCORES"]))
	hostname := strings.TrimSpace(sections["HOSTNAME"])
	if hostname == "" {
		hostname = s.IP()
	}
	c.JSON(200, gin.H{
		"hostname":   hostname,
		"os":         strings.TrimSpace(sections["OS"]),
		"kernel":     strings.TrimSpace(sections["KERNEL"]),
		"arch":       strings.TrimSpace(sections["ARCH"]),
		"cpu_model":  strings.TrimSpace(sections["CPUMODEL"]),
		"cpu_cores":  cores,
		"disks":      disks,
		"processes":  procs,
		"ssh_config": parseSSHConfig(sections["SSHCONFIG"]),
	})
}

// parseDisks 解析 ===DISK=== 段:兼容 df -hT(7 列,含文件系统类型)
// 与 df -h 回退输出(6 列,无类型)。
func parseDisks(raw string) []diskInfo {
	var disks []diskInfo
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[0] == "Filesystem" {
			continue
		}
		var d diskInfo
		if len(fields) >= 7 {
			d = diskInfo{Device: fields[0], FsType: fields[1], Size: fields[2],
				Used: fields[3], Avail: fields[4], UsePct: fields[5], Mount: fields[6]}
		} else {
			d = diskInfo{Device: fields[0], Size: fields[1], Used: fields[2],
				Avail: fields[3], UsePct: fields[4], Mount: fields[5]}
		}
		fmt.Sscanf(strings.TrimSuffix(d.UsePct, "%"), "%f", &d.UseNum)
		disks = append(disks, d)
	}
	return disks
}

// parseProcs 解析 ===TOPPROC=== 段(ps aux 风格)。BusyBox 等短行列数不足时跳过。
func parseProcs(raw string) []procInfo {
	var procs []procInfo
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 11 || fields[0] == "USER" {
			continue
		}
		cpuPct, _ := strconv.ParseFloat(fields[2], 64)
		memPct, _ := strconv.ParseFloat(fields[3], 64)
		procs = append(procs, procInfo{
			User: fields[0], PID: fields[1], CPUPct: cpuPct,
			MemPct: memPct, Command: strings.Join(fields[10:], " "),
		})
	}
	return procs
}

// parseSSHConfig 从 ===SSHCONFIG=== 段解析 sshd 转发相关配置。
// sshd -T 输出格式为 "allowtcpforwarding yes";配置文件格式为 "AllowTcpForwarding yes",
// 统一 lowercase 处理。SOURCE 行标识来源。
func parseSSHConfig(raw string) *sshConfigInfo {
	cfg := &sshConfigInfo{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 配置文件回退时 grep 输出带文件名前缀 "path:Keyword value",取冒号后内容。
		// 取第一个满足"冒号前含路径分隔符且无空格"的冒号作为前缀边界,
		// 以兼容值本身含冒号的项(如 permitopen host:2222)。
		for i := 0; i < len(line); i++ {
			if line[i] != ':' {
				continue
			}
			prefix := line[:i]
			if strings.Contains(prefix, "/") && !strings.Contains(prefix, " ") {
				line = strings.TrimSpace(line[i+1:])
			}
			break
		}
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "source="):
			cfg.Source = strings.TrimSpace(line[len("source="):])
		case strings.HasPrefix(lower, "allowtcpforwarding"):
			cfg.AllowTcpForwarding = fieldVal(lower)
		case strings.HasPrefix(lower, "disableforwarding"):
			cfg.DisableForwarding = fieldVal(lower)
		case strings.HasPrefix(lower, "permitopen"):
			cfg.PermitOpen = fieldVal(lower)
		case strings.HasPrefix(lower, "allowagentforwarding"):
			cfg.AllowAgentForward = fieldVal(lower)
		}
	}
	return cfg
}

// fieldVal 从 "keyword value" 格式中提取 value 部分。
func fieldVal(lowerLine string) string {
	parts := strings.Fields(lowerLine)
	if len(parts) < 2 {
		return ""
	}
	return strings.Join(parts[1:], " ")
}

// parseSections 将 ===KEY=== 分段格式解析为 map
func parseSections(output string) map[string]string {
	sections := make(map[string]string)
	var key, buf string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "===") && strings.HasSuffix(line, "===") {
			if key != "" {
				sections[key] = strings.TrimRight(buf, "\n")
			}
			key = strings.Trim(strings.TrimPrefix(strings.TrimSuffix(line, "==="), "==="), "=")
			buf = ""
		} else {
			buf += line + "\n"
		}
	}
	if key != "" {
		sections[key] = strings.TrimRight(buf, "\n")
	}
	return sections
}

// ---------- 动态指标 WebSocket(5s 推送) ----------

// metricsSampleBody 单次采样脚本,输出 ===KEY=== 分段(parseMetricsSample 解析)。
const metricsSampleBody = `echo "===STAT==="; head -1 /proc/stat
echo "===NET==="; awk '/:/{if ($1 != "lo:") {rx+=$2; tx+=$10}} END {print rx, tx}' /proc/net/dev
echo "===MEM==="; grep -E 'MemTotal|MemAvailable|SwapTotal|SwapFree' /proc/meminfo
echo "===LOAD==="; cat /proc/loadavg
echo "===UPTIME==="; cat /proc/uptime`

// metricsStreamScript 在远端循环采样:单条 exec 流复用通道,以 __SAMPLE__ 分隔
// 各样本,替代此前每 5 秒新建一条 SSH exec 通道的做法。
const metricsStreamScript = "while :; do\n" + metricsSampleBody + "\necho __SAMPLE__\nsleep 5\ndone"

// sampleMarker 样本分隔标记(独立于 ===KEY=== 段标记,二者不会冲突)。
var sampleMarker = []byte("__SAMPLE__")

var metricsUpgrader = newUpgrader(4096, 4096)

type metricsSample struct {
	statTotal   uint64
	statActive  uint64
	netRx       uint64
	netTx       uint64
	memTotalMB  float64
	memAvailMB  float64
	swapTotalMB float64
	swapFreeMB  float64
	load1m      float64
	load5m      float64
	load15m     float64
	uptimeSec   float64
	ts          time.Time
}

type metricsPayload struct {
	CPUPct        float64 `json:"cpu_pct"`
	MemTotalMB    float64 `json:"mem_total_mb"`
	MemUsedMB     float64 `json:"mem_used_mb"`
	MemPct        float64 `json:"mem_pct"`
	SwapTotalMB   float64 `json:"swap_total_mb"`
	SwapUsedMB    float64 `json:"swap_used_mb"`
	SwapPct       float64 `json:"swap_pct"`
	Load1m        float64 `json:"load_1m"`
	Load5m        float64 `json:"load_5m"`
	Load15m       float64 `json:"load_15m"`
	NetRxBps      float64 `json:"net_rx_bps"`
	NetTxBps      float64 `json:"net_tx_bps"`
	UptimeSeconds float64 `json:"uptime_seconds"`
}

func (a *API) metricsWS(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	ws, err := metricsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	b := newWSBridge(ws)
	defer b.close()

	stdout, cleanup, err := s.RunStream(metricsStreamScript)
	if err != nil {
		_ = b.write(websocket.TextMessage, []byte("错误: "+err.Error()))
		return
	}
	defer cleanup()

	go b.pumpPing(30 * time.Second)
	go b.readLoop(nil) // 客户端断开 → 关闭桥 → 主循环退出,defer 收尾流

	// 按样本标记切分流式输出,逐样本解析推送(CPU/NET 为相邻样本区间增量)
	var prev *metricsSample
	pending := make([]byte, 0, 64*1024)
	buf := make([]byte, 64*1024)
	for {
		n, readErr := stdout.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			for {
				idx := bytes.Index(pending, sampleMarker)
				if idx < 0 {
					break
				}
				cur := parseMetricsSample(string(pending[:idx]))
				pending = append(pending[:0], pending[idx+len(sampleMarker):]...)
				if cur == nil {
					continue
				}
				data, _ := json.Marshal(buildMetricsPayload(cur, prev))
				prev = cur
				if b.write(websocket.TextMessage, data) != nil {
					return
				}
			}
		}
		if readErr != nil {
			return // 远端流结束(会话断开或采样循环退出)
		}
	}
}

// buildMetricsPayload 由相邻两次采样计算推送载荷;prev 为 nil 时无 CPU/NET 增量。
func buildMetricsPayload(cur, prev *metricsSample) metricsPayload {
	p := metricsPayload{
		MemTotalMB: cur.memTotalMB,
		MemUsedMB:  cur.memTotalMB - cur.memAvailMB,
		Load1m:     cur.load1m, Load5m: cur.load5m, Load15m: cur.load15m,
		UptimeSeconds: cur.uptimeSec,
	}
	if p.MemTotalMB > 0 {
		p.MemPct = p.MemUsedMB / p.MemTotalMB * 100
	}
	p.SwapTotalMB = cur.swapTotalMB
	p.SwapUsedMB = cur.swapTotalMB - cur.swapFreeMB
	if cur.swapTotalMB > 0 {
		p.SwapPct = p.SwapUsedMB / cur.swapTotalMB * 100
	}
	if prev != nil {
		dt := cur.ts.Sub(prev.ts).Seconds()
		if dt > 0 {
			dTotal := float64(cur.statTotal) - float64(prev.statTotal)
			dActive := float64(cur.statActive) - float64(prev.statActive)
			if dTotal > 0 {
				p.CPUPct = dActive / dTotal * 100
				if p.CPUPct < 0 {
					p.CPUPct = 0
				}
				if p.CPUPct > 100 {
					p.CPUPct = 100
				}
			}
			dRx := float64(cur.netRx) - float64(prev.netRx)
			dTx := float64(cur.netTx) - float64(prev.netTx)
			if dRx > 0 {
				p.NetRxBps = dRx / dt
			}
			if dTx > 0 {
				p.NetTxBps = dTx / dt
			}
		}
	}
	return p
}

func parseMetricsSample(output string) *metricsSample {
	sections := parseSections(output)
	m := &metricsSample{ts: time.Now()}

	if line := strings.TrimSpace(sections["STAT"]); strings.HasPrefix(line, "cpu ") {
		fields := strings.Fields(strings.TrimPrefix(line, "cpu "))
		var v []uint64
		for _, f := range fields {
			n, _ := strconv.ParseUint(f, 10, 64)
			v = append(v, n)
		}
		if len(v) >= 5 {
			user := v[0] + v[1]       // user + nice
			sys := v[2] + v[5] + v[6] // system + irq + softirq
			idle := v[3] + v[4]       // idle + iowait
			var steal uint64
			if len(v) > 7 {
				steal = v[7]
			}
			m.statActive = user + sys + steal
			m.statTotal = m.statActive + idle
		}
	}

	if line := strings.TrimSpace(sections["NET"]); line != "" {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			m.netRx, _ = strconv.ParseUint(fields[0], 10, 64)
			m.netTx, _ = strconv.ParseUint(fields[1], 10, 64)
		}
	}

	for _, line := range strings.Split(sections["MEM"], "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		var val float64
		fmt.Sscanf(fields[1], "%f", &val)
		val /= 1024 // kB → MB
		switch {
		case strings.HasPrefix(fields[0], "MemTotal"):
			m.memTotalMB = val
		case strings.HasPrefix(fields[0], "MemAvailable"):
			m.memAvailMB = val
		case strings.HasPrefix(fields[0], "SwapTotal"):
			m.swapTotalMB = val
		case strings.HasPrefix(fields[0], "SwapFree"):
			m.swapFreeMB = val
		}
	}

	if line := strings.TrimSpace(sections["LOAD"]); line != "" {
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			m.load1m, _ = strconv.ParseFloat(fields[0], 64)
			m.load5m, _ = strconv.ParseFloat(fields[1], 64)
			m.load15m, _ = strconv.ParseFloat(fields[2], 64)
		}
	}

	if line := strings.TrimSpace(sections["UPTIME"]); line != "" {
		fields := strings.Fields(line)
		if len(fields) >= 1 {
			m.uptimeSec, _ = strconv.ParseFloat(fields[0], 64)
		}
	}

	return m
}
