package handler

// 主机信息采集:脚本常量与解析器统一收纳在 internal/opsview(Web 与 MCP
// 共用同一套,禁止复制第二套解析器);本文件仅保留
// Web handler 与包内类型/函数别名,行为与拆包前逐位一致。

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"webssh/internal/opsview"
)

// ---------- 与 opsview 的包内别名(含测试在内零改动引用) ----------

var hostInfoScript = opsview.HostInfoScript
var metricsStreamScript = opsview.MetricsStreamScript
var sampleMarker = opsview.SampleMarker

type diskInfo = opsview.DiskInfo
type procInfo = opsview.ProcInfo
type sshConfigInfo = opsview.SSHConfigInfo
type metricsSample = opsview.MetricsSample
type metricsPayload = opsview.MetricsPayload

func parseSections(output string) map[string]string { return opsview.ParseSections(output) }
func parseDisks(raw string) []diskInfo              { return opsview.ParseDisks(raw) }
func parseProcs(raw string) []procInfo              { return opsview.ParseProcs(raw) }
func parseSSHConfig(raw string) *sshConfigInfo      { return opsview.ParseSSHConfig(raw) }
func parseMetricsSample(output string) *metricsSample {
	return opsview.ParseMetricsSample(output)
}
func buildMetricsPayload(cur, prev *metricsSample) metricsPayload {
	return opsview.BuildMetricsPayload(cur, prev)
}

// ---------- 静态主机信息 handler ----------

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

// ---------- 动态指标 WebSocket(5s 推送) ----------

var metricsUpgrader = newUpgrader(4096, 4096)

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
