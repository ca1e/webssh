package opsview

import (
	"math"
	"testing"
	"time"
)

func TestParseSections(t *testing.T) {
	raw := `===A===
a1
a2
===B===

===C===
c1`
	sections := ParseSections(raw)
	if got := sections["A"]; got != "a1\na2" {
		t.Fatalf("sections[A] = %q", got)
	}
	if got := sections["B"]; got != "" {
		t.Fatalf("sections[B] = %q, want 空", got)
	}
	if got := sections["C"]; got != "c1" {
		t.Fatalf("sections[C] = %q", got)
	}
	if _, ok := sections["D"]; ok {
		t.Fatal("不应存在 sections[D]")
	}
}

func TestParseSSHConfig(t *testing.T) {
	raw := `allowtcpforwarding yes
SOURCE=sshd -T
/etc/ssh/sshd_config:PermitOpen host:2222
/etc/ssh/sshd_config.d/hardening.conf:AllowAgentForwarding no`
	cfg := ParseSSHConfig(raw)
	if cfg.AllowTcpForwarding != "yes" {
		t.Fatalf("AllowTcpForwarding = %q", cfg.AllowTcpForwarding)
	}
	if cfg.Source != "sshd -T" {
		t.Fatalf("Source = %q", cfg.Source)
	}
	// 值本身含冒号(permitopen host:2222)时前缀剥离不得吞掉值
	if cfg.PermitOpen != "host:2222" {
		t.Fatalf("PermitOpen = %q, want host:2222", cfg.PermitOpen)
	}
	if cfg.AllowAgentForward != "no" {
		t.Fatalf("AllowAgentForward = %q", cfg.AllowAgentForward)
	}
}

func TestParseDisks(t *testing.T) {
	// df -hT:7 列,含文件系统类型
	dfT := `Filesystem Type Size Used Avail Use% Mounted
/dev/sda1 ext4 100G 50G 50G 50% /
/dev/sdb1 xfs 10G 5G 5G 50% /data`
	disks := ParseDisks(dfT)
	if len(disks) != 2 {
		t.Fatalf("ParseDisks(df -hT) 条数 = %d, want 2", len(disks))
	}
	if disks[0].Device != "/dev/sda1" || disks[0].FsType != "ext4" ||
		disks[0].Size != "100G" || disks[0].Mount != "/" || disks[0].UseNum != 50 {
		t.Fatalf("ParseDisks(df -hT)[0] = %+v", disks[0])
	}

	// df -h 回退:6 列,无类型
	dfH := `Filesystem Size Used Avail Use% Mounted
/dev/sda1 100G 50G 50G 50% /`
	disks = ParseDisks(dfH)
	if len(disks) != 1 {
		t.Fatalf("ParseDisks(df -h) 条数 = %d, want 1", len(disks))
	}
	if disks[0].FsType != "" || disks[0].Device != "/dev/sda1" ||
		disks[0].Size != "100G" || disks[0].Mount != "/" || disks[0].UseNum != 50 {
		t.Fatalf("ParseDisks(df -h)[0] = %+v", disks[0])
	}

	// 空段与列数不足的行跳过
	if got := ParseDisks(""); len(got) != 0 {
		t.Fatalf("ParseDisks(\"\") = %+v", got)
	}
	if got := ParseDisks("/dev/sda1 ext4 100G"); len(got) != 0 {
		t.Fatalf("ParseDisks(短行) = %+v", got)
	}
}

func TestParseProcs(t *testing.T) {
	raw := `USER               PID  %CPU %MEM      VSZ    RSS TTY      STAT START   TIME COMMAND
root             1  0.5  1.2 123456  67890 ?        Ss   10:00   0:01 /sbin/init
user            42  2.5  0.3    999    111 pts/0    R    10:01   0:00 ps aux --sort=-%cpu`
	procs := ParseProcs(raw)
	if len(procs) != 2 {
		t.Fatalf("parseProcs 条数 = %d, want 2", len(procs))
	}
	if procs[0].User != "root" || procs[0].PID != "1" || procs[0].CPUPct != 0.5 ||
		procs[0].MemPct != 1.2 || procs[0].Command != "/sbin/init" {
		t.Fatalf("parseProcs[0] = %+v", procs[0])
	}
	// 命令含空格时合并为一列
	if procs[1].Command != "ps aux --sort=-%cpu" {
		t.Fatalf("parseProcs[1].Command = %q", procs[1].Command)
	}
}

func TestParseMetricsSample(t *testing.T) {
	raw := `===STAT===
cpu  100 200 300 4000 500 60 70 80 0 0
===NET===
1234 5678
===MEM===
MemTotal:        2048000 kB
MemAvailable:    1024000 kB
SwapTotal:        512000 kB
SwapFree:         256000 kB
===LOAD===
1.50 0.50 0.10 1/100 1234
===UPTIME===
12345.67 23456.78`
	m := ParseMetricsSample(raw)
	if m == nil {
		t.Fatal("parseMetricsSample 返回 nil")
	}
	// stat: user+nice=300, system+irq+softirq=430, idle+iowait=4500, steal=80
	if m.StatActive != 810 || m.StatTotal != 5310 {
		t.Fatalf("statActive/Total = %d/%d, want 810/5310", m.StatActive, m.StatTotal)
	}
	if m.NetRx != 1234 || m.NetTx != 5678 {
		t.Fatalf("netRx/Tx = %d/%d", m.NetRx, m.NetTx)
	}
	if m.MemTotalMB != 2000 || m.MemAvailMB != 1000 || m.SwapTotalMB != 500 || m.SwapFreeMB != 250 {
		t.Fatalf("mem = %+v", m)
	}
	if m.Load1m != 1.5 || m.Load5m != 0.5 || m.Load15m != 0.1 {
		t.Fatalf("load = %+v", m)
	}
	if m.UptimeSec != 12345.67 {
		t.Fatalf("uptime = %v", m.UptimeSec)
	}

	// 缺关键段时不报错,返回零值样本
	if m := ParseMetricsSample("===LOAD===\n1.0 0 0"); m == nil {
		t.Fatal("局部样本不应返回 nil")
	}
}

func TestBuildMetricsPayload(t *testing.T) {
	base := time.Now()
	prev := &MetricsSample{
		StatTotal: 4310, StatActive: 10,
		NetRx: 234, NetTx: 5178,
		MemTotalMB: 2000, MemAvailMB: 1000, SwapTotalMB: 500, SwapFreeMB: 250,
		Load1m: 1.5, Load5m: 0.5, Load15m: 0.1, UptimeSec: 12335.67,
		TS: base,
	}
	cur := &MetricsSample{
		StatTotal: 5310, StatActive: 810,
		NetRx: 1234, NetTx: 5678,
		MemTotalMB: 2000, MemAvailMB: 1000, SwapTotalMB: 500, SwapFreeMB: 250,
		Load1m: 1.5, Load5m: 0.5, Load15m: 0.1, UptimeSec: 12345.67,
		TS: base.Add(10 * time.Second),
	}
	p := BuildMetricsPayload(cur, prev)
	// CPU:(810-10)/(5310-4310)*100 = 80%
	if math.Abs(p.CPUPct-80) > 1e-9 {
		t.Fatalf("CPUPct = %v, want 80", p.CPUPct)
	}
	if math.Abs(p.NetRxBps-100) > 1e-9 || math.Abs(p.NetTxBps-50) > 1e-9 {
		t.Fatalf("Net = %v/%v, want 100/50", p.NetRxBps, p.NetTxBps)
	}
	if p.MemTotalMB != 2000 || p.MemUsedMB != 1000 || math.Abs(p.MemPct-50) > 1e-9 {
		t.Fatalf("Mem = %+v", p)
	}
	if p.SwapTotalMB != 500 || p.SwapUsedMB != 250 || math.Abs(p.SwapPct-50) > 1e-9 {
		t.Fatalf("Swap = %+v", p)
	}
	if math.Abs(p.UptimeSeconds-12345.67) > 1e-9 {
		t.Fatalf("Uptime = %v", p.UptimeSeconds)
	}

	// 首个样本无 prev:无 CPU/NET 增量
	first := BuildMetricsPayload(cur, nil)
	if first.CPUPct != 0 || first.NetRxBps != 0 || first.NetTxBps != 0 {
		t.Fatalf("首个样本不应有增量: %+v", first)
	}
}
