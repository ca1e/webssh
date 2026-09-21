// Package opsview 收纳 Web 端与 MCP 端共用的远端采集脚本与纯解析函数
// (Web 与 MCP 的监控视图必须复用同一套解析器,禁止复制第二套)。
// 包内零依赖(gin/sshmgr 均不导入),输出结构即 JSON 契约,Web 前端与
// MCP 工具共享同一字段形态。
package opsview

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ---------- 主机静态信息(hostInfo / ssh_system_info) ----------

// HostInfoScriptCore 采集主机静态信息,输出 ===KEY=== 分段;各命令均带回退
// 分支,兼容 BusyBox/无 GNU 工具的环境(df 无 -T、ps 无 --sort 时退化为无
// 类型/无排序输出,解析端对两种列数均容忍)。
const HostInfoScriptCore = `echo "===HOSTNAME==="
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
fi`

// TopProcSection 进程段:仅 Web 端 hostInfo 使用;MCP ssh_system_info 明确
// 去除 topproc(进程视图由 ps_top 模板按管理员配置提供)。
const TopProcSection = `
echo "===TOPPROC==="
{ ps aux --sort=-%cpu 2>/dev/null || ps aux 2>/dev/null || ps w; } | head -21`

// HostInfoScript Web 端完整采集脚本(含 TOPPROC 段)。
const HostInfoScript = HostInfoScriptCore + TopProcSection

// DiskListCmd 磁盘列表命令(ssh_disk_usage 与 DISK 段同源,不复制第二份)。
const DiskListCmd = `{ LC_ALL=C df -hT 2>/dev/null || LC_ALL=C df -h 2>/dev/null; } | grep -vE 'tmpfs|devtmpfs|overlay'`

// ---------- 动态指标采样(metricsWS / ssh_cpu_memory / ssh_network_status) ----------

// MetricsSampleBody 单次采样脚本,输出 ===KEY=== 分段(ParseMetricsSample 解析)。
const MetricsSampleBody = `echo "===STAT==="; head -1 /proc/stat
echo "===NET==="; awk '/:/{if ($1 != "lo:") {rx+=$2; tx+=$10}} END {print rx, tx}' /proc/net/dev
echo "===MEM==="; grep -E 'MemTotal|MemAvailable|SwapTotal|SwapFree' /proc/meminfo
echo "===LOAD==="; cat /proc/loadavg
echo "===UPTIME==="; cat /proc/uptime`

// MetricsStreamScript 在远端循环采样:单条 exec 流复用通道,以 __SAMPLE__
// 分隔各样本,替代每 5 秒新建一条 SSH exec 通道。
const MetricsStreamScript = "while :; do\n" + MetricsSampleBody + "\necho __SAMPLE__\nsleep 5\ndone"

// SampleMarker 样本分隔标记(独立于 ===KEY=== 段标记,二者不会冲突)。
var SampleMarker = []byte("__SAMPLE__")

// DiskInfo 一块磁盘的使用情况(兼容 df -hT 七列与 df -h 六列)。
type DiskInfo struct {
	Device string  `json:"device"`
	FsType string  `json:"fs_type"`
	Size   string  `json:"size"`
	Used   string  `json:"used"`
	Avail  string  `json:"avail"`
	UsePct string  `json:"use_pct"`
	Mount  string  `json:"mount"`
	UseNum float64 `json:"use_num"`
}

// ProcInfo ps aux 风格的单进程条目。
type ProcInfo struct {
	User    string  `json:"user"`
	PID     string  `json:"pid"`
	CPUPct  float64 `json:"cpu_pct"`
	MemPct  float64 `json:"mem_pct"`
	Command string  `json:"command"`
}

// SSHConfigInfo 汇总与转发能力直接相关的 sshd 配置项。
type SSHConfigInfo struct {
	AllowTcpForwarding string `json:"allow_tcp_forwarding"` // yes/no/local/remote，空串=未知
	DisableForwarding  string `json:"disable_forwarding"`   // yes/no
	PermitOpen         string `json:"permit_open"`          // any 或 host:port 列表
	AllowAgentForward  string `json:"allow_agent_forwarding"`
	Source             string `json:"source"` // "sshd -T" | "config-file" | ""
}

// MetricsSample 一次采样的原始数值。
type MetricsSample struct {
	StatTotal   uint64
	StatActive  uint64
	NetRx       uint64
	NetTx       uint64
	MemTotalMB  float64
	MemAvailMB  float64
	SwapTotalMB float64
	SwapFreeMB  float64
	Load1m      float64
	Load5m      float64
	Load15m     float64
	UptimeSec   float64
	TS          time.Time
}

// MetricsPayload 推送/返回给消费方的指标载荷。
type MetricsPayload struct {
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

// ParseSections 将 ===KEY=== 分段格式解析为 map。
func ParseSections(output string) map[string]string {
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

// ParseDisks 解析 ===DISK=== 段:兼容 df -hT(7 列,含文件系统类型)
// 与 df -h 回退输出(6 列,无类型)。
func ParseDisks(raw string) []DiskInfo {
	var disks []DiskInfo
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[0] == "Filesystem" {
			continue
		}
		var d DiskInfo
		if len(fields) >= 7 {
			d = DiskInfo{Device: fields[0], FsType: fields[1], Size: fields[2],
				Used: fields[3], Avail: fields[4], UsePct: fields[5], Mount: fields[6]}
		} else {
			d = DiskInfo{Device: fields[0], Size: fields[1], Used: fields[2],
				Avail: fields[3], UsePct: fields[4], Mount: fields[5]}
		}
		fmt.Sscanf(strings.TrimSuffix(d.UsePct, "%"), "%f", &d.UseNum)
		disks = append(disks, d)
	}
	return disks
}

// ParseProcs 解析 ===TOPPROC=== 段(ps aux 风格)。BusyBox 等短行列数不足时跳过。
func ParseProcs(raw string) []ProcInfo {
	var procs []ProcInfo
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 11 || fields[0] == "USER" {
			continue
		}
		cpuPct, _ := strconv.ParseFloat(fields[2], 64)
		memPct, _ := strconv.ParseFloat(fields[3], 64)
		procs = append(procs, ProcInfo{
			User: fields[0], PID: fields[1], CPUPct: cpuPct,
			MemPct: memPct, Command: strings.Join(fields[10:], " "),
		})
	}
	return procs
}

// ParseSSHConfig 从 ===SSHCONFIG=== 段解析 sshd 转发相关配置。
// sshd -T 输出格式为 "allowtcpforwarding yes";配置文件格式为
// "AllowTcpForwarding yes",统一 lowercase 处理。SOURCE 行标识来源。
func ParseSSHConfig(raw string) *SSHConfigInfo {
	cfg := &SSHConfigInfo{}
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

// ParseMetricsSample 解析 MetricsSampleBody 的分段输出为原始数值;
// prev 不参与(由 BuildMetricsPayload 做差分)。
func ParseMetricsSample(output string) *MetricsSample {
	sections := ParseSections(output)
	m := &MetricsSample{TS: time.Now()}

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
			m.StatActive = user + sys + steal
			m.StatTotal = m.StatActive + idle
		}
	}

	if line := strings.TrimSpace(sections["NET"]); line != "" {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			m.NetRx, _ = strconv.ParseUint(fields[0], 10, 64)
			m.NetTx, _ = strconv.ParseUint(fields[1], 10, 64)
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
			m.MemTotalMB = val
		case strings.HasPrefix(fields[0], "MemAvailable"):
			m.MemAvailMB = val
		case strings.HasPrefix(fields[0], "SwapTotal"):
			m.SwapTotalMB = val
		case strings.HasPrefix(fields[0], "SwapFree"):
			m.SwapFreeMB = val
		}
	}

	if line := strings.TrimSpace(sections["LOAD"]); line != "" {
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			m.Load1m, _ = strconv.ParseFloat(fields[0], 64)
			m.Load5m, _ = strconv.ParseFloat(fields[1], 64)
			m.Load15m, _ = strconv.ParseFloat(fields[2], 64)
		}
	}

	if line := strings.TrimSpace(sections["UPTIME"]); line != "" {
		fields := strings.Fields(line)
		if len(fields) >= 1 {
			m.UptimeSec, _ = strconv.ParseFloat(fields[0], 64)
		}
	}

	return m
}

// BuildMetricsPayload 由相邻两次采样计算推送载荷;prev 为 nil 时无 CPU/NET
// 增量。计算逻辑与原 handler 实现逐位一致(夹紧/回绕保护/不取整),保证
// Web 端推送行为零变化。MCP 单次调用场景的单样本 CPU 占比见 SingleSampleCPU。
func BuildMetricsPayload(cur, prev *MetricsSample) MetricsPayload {
	p := MetricsPayload{
		MemTotalMB: cur.MemTotalMB,
		MemUsedMB:  cur.MemTotalMB - cur.MemAvailMB,
		Load1m:     cur.Load1m, Load5m: cur.Load5m, Load15m: cur.Load15m,
		UptimeSeconds: cur.UptimeSec,
	}
	if p.MemTotalMB > 0 {
		p.MemPct = p.MemUsedMB / p.MemTotalMB * 100
	}
	p.SwapTotalMB = cur.SwapTotalMB
	p.SwapUsedMB = cur.SwapTotalMB - cur.SwapFreeMB
	if cur.SwapTotalMB > 0 {
		p.SwapPct = p.SwapUsedMB / cur.SwapTotalMB * 100
	}
	if prev != nil {
		dt := cur.TS.Sub(prev.TS).Seconds()
		if dt > 0 {
			dTotal := float64(cur.StatTotal) - float64(prev.StatTotal)
			dActive := float64(cur.StatActive) - float64(prev.StatActive)
			if dTotal > 0 {
				p.CPUPct = dActive / dTotal * 100
				if p.CPUPct < 0 {
					p.CPUPct = 0
				}
				if p.CPUPct > 100 {
					p.CPUPct = 100
				}
			}
			dRx := float64(cur.NetRx) - float64(prev.NetRx)
			dTx := float64(cur.NetTx) - float64(prev.NetTx)
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

// SingleSampleCPU 单样本 CPU 占比(自启动以来均值),供 MCP 无 WS 流上下文
// 的单次调用使用(CPU 用单样本 /proc/stat 占比而非相邻差值)。
func SingleSampleCPU(m *MetricsSample) float64 {
	if m == nil || m.StatTotal == 0 {
		return 0
	}
	return float64(m.StatActive) / float64(m.StatTotal) * 100
}
