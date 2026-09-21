package mcp

// monitor 域工具:全部"单次调用"语义,复用 opsview 的采集脚本与解析器
// (与 Web 端同一套,禁止第二套解析器)。
// 全部只读;服务启停、进程 kill 不在本工具面。

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"webssh/internal/opsview"
	"webssh/internal/sshmgr"
)

// shQuote 单引号转义:与 sshmgr/policy 同模式(此处的目标命令由服务端
// 常量拼装,仅服务名参数经此转义)。
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sampleTwice 间隔 interval 连采两次动态指标(第二次与第一次做差分,
// 得到真实区间速率而非自启动均值)。
func sampleTwice(ctx context.Context, sess *sshmgr.Session, interval time.Duration) (cur, prev *opsview.MetricsSample, err error) {
	sample := func() (*opsview.MetricsSample, error) {
		out, err := sess.Run(opsview.MetricsSampleBody)
		if err != nil && out == "" {
			return nil, fmt.Errorf("指标采样失败:%v", err)
		}
		return opsview.ParseMetricsSample(out), nil
	}
	if prev, err = sample(); err != nil {
		return nil, nil, err
	}
	select {
	case <-ctx.Done():
		return nil, nil, fmt.Errorf("采样被中止")
	case <-time.After(interval):
	}
	if cur, err = sample(); err != nil {
		return nil, nil, err
	}
	return cur, prev, nil
}

func (s *Server) toolSystemInfo(ctx context.Context, req *mcp.CallToolRequest, in hostIn) (*mcp.CallToolResult, any, error) {
	start := time.Now()
	var out any
	var sess *sshmgr.Session
	err := func() error {
		h, err := s.resolveHost(in.Host)
		if err != nil {
			return err
		}
		if err = rejectTelnet(h); err != nil {
			return err
		}
		sess, err = s.acquire(ctx, h)
		if err != nil {
			return err
		}
		// HostInfoScriptCore 无 TOPPROC 段(进程视图按需另行采集)
		res, err := sess.RunExec(ctx, opsview.HostInfoScriptCore, s.conf.Defaults.ReadMaxBytes)
		if err != nil {
			return fmt.Errorf("采集主机信息失败:%v", err)
		}
		sections := opsview.ParseSections(res.Stdout)
		hostname := strings.TrimSpace(sections["HOSTNAME"])
		if hostname == "" {
			hostname = h.Host
		}
		out = map[string]any{
			"hostname":   hostname,
			"os":         strings.TrimSpace(sections["OS"]),
			"kernel":     strings.TrimSpace(sections["KERNEL"]),
			"arch":       strings.TrimSpace(sections["ARCH"]),
			"cpu_model":  strings.TrimSpace(sections["CPUMODEL"]),
			"cpu_cores":  strings.TrimSpace(sections["CPUCORES"]),
			"disks":      opsview.ParseDisks(sections["DISK"]),
			"ssh_config": opsview.ParseSSHConfig(sections["SSHCONFIG"]),
			"truncated":  res.Truncated,
		}
		return nil
	}()
	release(sess)
	defer func() { callLog("ssh_system_info", in.Host, err, start) }()
	return nil, out, err
}

func (s *Server) toolCPUMemory(ctx context.Context, req *mcp.CallToolRequest, in hostIn) (*mcp.CallToolResult, any, error) {
	start := time.Now()
	var out any
	var sess *sshmgr.Session
	err := func() error {
		h, err := s.resolveHost(in.Host)
		if err != nil {
			return err
		}
		if err = rejectTelnet(h); err != nil {
			return err
		}
		sess, err = s.acquire(ctx, h)
		if err != nil {
			return err
		}
		cur, prev, err := sampleTwice(ctx, sess, time.Second)
		if err != nil {
			return err
		}
		payload := opsview.BuildMetricsPayload(cur, prev)
		out = map[string]any{
			"cpu_pct":        payload.CPUPct,
			"mem_total_mb":   payload.MemTotalMB,
			"mem_used_mb":    payload.MemUsedMB,
			"mem_pct":        payload.MemPct,
			"swap_total_mb":  payload.SwapTotalMB,
			"swap_used_mb":   payload.SwapUsedMB,
			"swap_pct":       payload.SwapPct,
			"load_1m":        payload.Load1m,
			"load_5m":        payload.Load5m,
			"load_15m":       payload.Load15m,
			"uptime_seconds": payload.UptimeSeconds,
			"sampled_at":     cur.TS.Format(time.RFC3339),
		}
		return nil
	}()
	release(sess)
	defer func() { callLog("ssh_cpu_memory", in.Host, err, start) }()
	return nil, out, err
}

func (s *Server) toolNetworkStatus(ctx context.Context, req *mcp.CallToolRequest, in hostIn) (*mcp.CallToolResult, any, error) {
	start := time.Now()
	var out any
	var sess *sshmgr.Session
	err := func() error {
		h, err := s.resolveHost(in.Host)
		if err != nil {
			return err
		}
		if err = rejectTelnet(h); err != nil {
			return err
		}
		sess, err = s.acquire(ctx, h)
		if err != nil {
			return err
		}
		cur, prev, err := sampleTwice(ctx, sess, time.Second)
		if err != nil {
			return err
		}
		payload := opsview.BuildMetricsPayload(cur, prev)
		// 接口地址复用 ip_addr 模板命令(与 exec 域同源,不复制命令文本)
		ifaces, _ := sess.Run("ip -brief addr 2>/dev/null || ifconfig -a 2>/dev/null || ip addr")
		out = map[string]any{
			"rx_bytes_total": cur.NetRx,
			"tx_bytes_total": cur.NetTx,
			"rx_bps":         payload.NetRxBps,
			"tx_bps":         payload.NetTxBps,
			"load_1m":        payload.Load1m,
			"uptime_seconds": payload.UptimeSeconds,
			"interfaces":     strings.TrimRight(ifaces, "\n"),
			"sampled_at":     cur.TS.Format(time.RFC3339),
		}
		return nil
	}()
	release(sess)
	defer func() { callLog("ssh_network_status", in.Host, err, start) }()
	return nil, out, err
}

func (s *Server) toolDiskUsage(ctx context.Context, req *mcp.CallToolRequest, in hostIn) (*mcp.CallToolResult, any, error) {
	start := time.Now()
	var out any
	var sess *sshmgr.Session
	err := func() error {
		h, err := s.resolveHost(in.Host)
		if err != nil {
			return err
		}
		if err = rejectTelnet(h); err != nil {
			return err
		}
		sess, err = s.acquire(ctx, h)
		if err != nil {
			return err
		}
		// DiskListCmd 与 Web 端 DISK 段同源
		res, err := sess.RunExec(ctx, opsview.DiskListCmd, s.conf.Defaults.ReadMaxBytes)
		if err != nil {
			return fmt.Errorf("磁盘采集失败:%v", err)
		}
		disks := opsview.ParseDisks(res.Stdout)
		if disks == nil {
			disks = []opsview.DiskInfo{}
		}
		out = map[string]any{"disks": disks, "truncated": res.Truncated}
		return nil
	}()
	release(sess)
	defer func() { callLog("ssh_disk_usage", in.Host, err, start) }()
	return nil, out, err
}

type serviceStatusIn struct {
	Host string `json:"host" jsonschema:"主机名称或 ID"`
	Name string `json:"name" jsonschema:"systemd 单元名,如 sshd 或 sshd.service"`
}

// unitNameRe systemd 单元名白名单。
var unitNameRe = regexp.MustCompile(`^[a-zA-Z0-9@:._-]+(\.service)?$`)

func (s *Server) toolServiceStatus(ctx context.Context, req *mcp.CallToolRequest, in serviceStatusIn) (*mcp.CallToolResult, any, error) {
	start := time.Now()
	var out any
	var sess *sshmgr.Session
	err := func() error {
		h, err := s.resolveHost(in.Host)
		if err != nil {
			return err
		}
		if err = rejectTelnet(h); err != nil {
			return err
		}
		name := strings.TrimSpace(in.Name)
		if !unitNameRe.MatchString(name) {
			return fmt.Errorf("参数 name 值 %q 不符合 ^[a-zA-Z0-9@:._-]+(\\.service)?$", in.Name)
		}
		sess, err = s.acquire(ctx, h)
		if err != nil {
			return err
		}
		// 只读 show:ActiveState/SubState/描述/启动时间/主 PID/重启次数
		res, err := sess.RunExec(ctx, fmt.Sprintf("systemctl show -- %s -p ActiveState,SubState,Description,ExecMainStartTimestamp,MainPID,NRestarts", shQuote(name)), s.conf.Defaults.ReadMaxBytes)
		if err != nil {
			return fmt.Errorf("查询服务状态失败:%v", err)
		}
		props := map[string]string{}
		for _, line := range strings.Split(res.Stdout, "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
				props[k] = v
			}
		}
		out = map[string]any{
			"name":         name,
			"active_state": props["ActiveState"],
			"sub_state":    props["SubState"],
			"description":  props["Description"],
			"started_at":   props["ExecMainStartTimestamp"],
			"main_pid":     props["MainPID"],
			"n_restarts":   props["NRestarts"],
		}
		return nil
	}()
	release(sess)
	defer func() { callLog("ssh_service_status", in.Host+"#"+in.Name, err, start) }()
	return nil, out, err
}

func (s *Server) toolDockerStatus(ctx context.Context, req *mcp.CallToolRequest, in hostIn) (*mcp.CallToolResult, any, error) {
	start := time.Now()
	var out any
	var sess *sshmgr.Session
	err := func() error {
		h, err := s.resolveHost(in.Host)
		if err != nil {
			return err
		}
		if err = rejectTelnet(h); err != nil {
			return err
		}
		sess, err = s.acquire(ctx, h)
		if err != nil {
			return err
		}
		verOut, err := sess.Run(opsview.DockerCheckCmd)
		if err != nil {
			return fmt.Errorf("Docker 检测失败:%v", err)
		}
		if strings.TrimSpace(verOut) == "" {
			out = map[string]any{"available": false, "containers": []opsview.Container{}}
			return nil
		}
		listOut, _ := sess.Run(opsview.DockerListCmd)
		containers := opsview.ParseContainerList(listOut)
		if containers == nil {
			containers = []opsview.Container{}
		}
		infoOut, _ := sess.Run(opsview.DockerInfoScript)
		info, infoOK, compose, infoErr := opsview.ParseDockerInfoScript(infoOut)
		res := map[string]any{"available": true, "containers": containers}
		if infoOK {
			res["info"] = info
			res["compose"] = compose
		} else {
			res["info_error"] = infoErr
		}
		out = res
		return nil
	}()
	release(sess)
	defer func() { callLog("ssh_docker_status", in.Host, err, start) }()
	return nil, out, err
}

func (s *Server) toolK8sStatus(ctx context.Context, req *mcp.CallToolRequest, in hostIn) (*mcp.CallToolResult, any, error) {
	start := time.Now()
	var out any
	var sess *sshmgr.Session
	err := func() error {
		h, err := s.resolveHost(in.Host)
		if err != nil {
			return err
		}
		if err = rejectTelnet(h); err != nil {
			return err
		}
		sess, err = s.acquire(ctx, h)
		if err != nil {
			return err
		}
		// 缓存探测(force=false):单次调用语义,不触发强制刷新
		state := s.k8s.State(sess, false)
		out = state
		return nil
	}()
	release(sess)
	defer func() { callLog("ssh_k8s_status", in.Host, err, start) }()
	return nil, out, err
}
