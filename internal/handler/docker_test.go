package handler

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSanitizeDockerID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"abc123DEF-4.5_x", "abc123DEF-4.5_x"},
		{"a'b$(id)", "abid"},
		{"a`id`b;rm -rf /", "aidbrm-rf"},
		{"容器 id", "id"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := sanitizeDockerID(tc.in); got != tc.want {
			t.Fatalf("sanitizeDockerID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"abc", "'abc'"},
		{"", "''"},
		{"it's", `'it'\''s'`},
		{"a b;c", "'a b;c'"},
		{"$(id)", "'$(id)'"},
	}
	for _, tc := range cases {
		if got := shellQuote(tc.in); got != tc.want {
			t.Fatalf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseDockerTime(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"2024-01-15 10:30:00 +0800 CST", "2024-01-15T10:30:00+08:00"},
		{"2024-01-15 10:30:00 +0000 UTC", "2024-01-15T10:30:00Z"},
		{"2026-05-01 08:00:00 -0500 EDT", "2026-05-01T08:00:00-05:00"},
	}
	for _, tc := range cases {
		if got := parseDockerTime(tc.in); got != tc.want {
			t.Fatalf("parseDockerTime(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// 无时区:按本地时区解析,与 time.Date 独立构造的期望值比对
	want := time.Date(2024, 1, 15, 10, 30, 0, 0, time.Local).Format(time.RFC3339)
	if got := parseDockerTime("2024-01-15 10:30:00"); got != want {
		t.Fatalf("parseDockerTime 无时区解析不符: got %q, want %q", got, want)
	}

	// 非法输入原样返回
	if got := parseDockerTime("not-a-time"); got != "not-a-time" {
		t.Fatalf("parseDockerTime 非法输入应原样返回,得到 %q", got)
	}
}

// infoSect 按 dockerInfoScript 的 emit 协议构造一段标记输出。
func infoSect(name, status, body string) string {
	return fmt.Sprintf("__WS_%s_%s__\n%s\n__WS_END__\n", name, status, body)
}

func TestParseDockerInfoScriptComposeV2(t *testing.T) {
	out := infoSect("INFO", "OK",
		`{"ServerVersion":"26.1.5+dfsg1","Driver":"overlay2","OSType":"linux","Architecture":"x86_64","OperatingSystem":"Ubuntu 22.04","Containers":5,"ContainersRunning":3,"ContainersPaused":0,"ContainersStopped":2,"Images":10}`) +
		infoSect("VER", "OK",
			`{"Client":{"Version":"26.1.5","ApiVersion":"1.45","Os":"linux","Arch":"amd64"},"Server":{"Version":"26.1.5+dfsg1","ApiVersion":"1.45"}}`) +
		infoSect("CV2", "OK", "2.24.5") +
		infoSect("CLS", "OK", `[{"Name":"webssh","Status":"running(2)","ConfigFiles":"/opt/webssh/compose.yaml"},{"Name":"blog","Status":"exited(1)","ConfigFiles":"/srv/blog/docker-compose.yml"}]`)
	info, ok, compose, infoErr := parseDockerInfoScript(out)
	if !ok || infoErr != "" {
		t.Fatalf("INFO OK 段应可用,ok=%v err=%q", ok, infoErr)
	}
	want := dockerServerInfo{
		ClientVersion: "26.1.5", ServerVersion: "26.1.5+dfsg1", APIVersion: "1.45",
		StorageDriver: "overlay2",
		OSType: "linux", Architecture: "x86_64", OS: "Ubuntu 22.04",
		Containers: 5, Running: 3, Paused: 0, Stopped: 2, Images: 10,
	}
	if info != want {
		t.Fatalf("info 字段映射不符:\n got %+v\nwant %+v", info, want)
	}
	if compose.Command != "docker compose" || compose.Version != "2.24.5" {
		t.Fatalf("compose 命令/版本不符: %+v", compose)
	}
	if len(compose.Projects) != 2 || compose.Projects[0].Name != "webssh" ||
		compose.Projects[0].Status != "running(2)" ||
		compose.Projects[1].ConfigFiles != "/srv/blog/docker-compose.yml" {
		t.Fatalf("compose 项目解析不符: %+v", compose.Projects)
	}
}

func TestParseDockerInfoScriptVerFallback(t *testing.T) {
	// VER 段缺失(老版本 docker version 无 json 输出等):退回 info 的 ServerVersion
	out := infoSect("INFO", "OK", `{"ServerVersion":"20.10.7","Containers":1,"ContainersRunning":1,"Images":2}`)
	info, ok, _, _ := parseDockerInfoScript(out)
	if !ok {
		t.Fatal("INFO OK 时应可用")
	}
	if info.ServerVersion != "20.10.7" || info.ClientVersion != "" || info.APIVersion != "" {
		t.Fatalf("VER 缺失时应退回 info 的 ServerVersion: %+v", info)
	}
}

func TestParseDockerInfoScriptComposeV1(t *testing.T) {
	// 旧版 docker-compose:CV1 报版本;ls --format json 不被支持时 CLS 为 ERR、无输出
	out := infoSect("INFO", "OK", `{"ServerVersion":"20.10.7","Containers":1,"ContainersRunning":1,"Images":2}`) +
		infoSect("CV1", "OK", "1.29.2") +
		infoSect("CLS", "ERR", "")
	_, ok, compose, _ := parseDockerInfoScript(out)
	if !ok {
		t.Fatal("INFO OK 时应可用")
	}
	if compose.Command != "docker-compose" || compose.Version != "1.29.2" {
		t.Fatalf("V1 命令/版本不符: %+v", compose)
	}
	if len(compose.Projects) != 0 {
		t.Fatalf("CLS ERR 时项目应为空: %+v", compose.Projects)
	}
}

func TestParseDockerInfoScriptCLSNonJSON(t *testing.T) {
	// CLS 输出非 JSON(旧版表格)应退化为空列表而非报错
	out := infoSect("INFO", "OK", `{"ServerVersion":"24.0.7"}`) +
		infoSect("CV2", "OK", "2.24.5") +
		infoSect("CLS", "OK", "NAME                 STATUS")
	_, _, compose, _ := parseDockerInfoScript(out)
	if len(compose.Projects) != 0 {
		t.Fatalf("非 JSON 项目列表应退化为空: %+v", compose.Projects)
	}
}

func TestParseDockerInfoScriptUnavailable(t *testing.T) {
	// docker 不可用:INFO ERR 透出报错,compose 段即使 OK 也不视为可用
	out := infoSect("INFO", "ERR", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?") +
		infoSect("CV2", "OK", "2.24.5")
	info, ok, _, infoErr := parseDockerInfoScript(out)
	if ok {
		t.Fatal("INFO ERR 时不应可用")
	}
	if !strings.Contains(infoErr, "Cannot connect to the Docker daemon") {
		t.Fatalf("错误原因应取 INFO 段首行,得到 %q", infoErr)
	}
	if info.ServerVersion != "" {
		t.Fatalf("不可用时 info 应为零值: %+v", info)
	}
}

func TestParseDockerInfoScriptNoCompose(t *testing.T) {
	// docker 可用但 compose 两种形态都缺失:CVER ERR,docker 信息仍可用
	out := infoSect("INFO", "OK", `{"ServerVersion":"24.0.7","Containers":0,"Images":0}`) +
		infoSect("CVER", "ERR", "docker compose 与 docker-compose 均未安装")
	_, ok, compose, _ := parseDockerInfoScript(out)
	if !ok {
		t.Fatal("仅 compose 缺失时 docker 信息应可用")
	}
	if compose.Command != "" || compose.Version != "" || len(compose.Projects) != 0 {
		t.Fatalf("compose 未安装时卡片数据应为空: %+v", compose)
	}
}
