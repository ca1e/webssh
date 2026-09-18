package handler

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ---------- Docker 管理(仅查看) ----------

const dockerCheckCmd = `which docker >/dev/null 2>&1 && docker info --format '{{.ServerVersion}}' 2>/dev/null || echo ""`

const dockerListCmd = `docker ps -a --format '{{json .}}' 2>/dev/null`

type dockerContainer struct {
	ID        string `json:"id"`       // 完整 ID(用于 inspect)
	ShortID   string `json:"short_id"` // 前12位(用于显示)
	Name      string `json:"name"`
	Image     string `json:"image"`
	Command   string `json:"command"`
	CreatedAt string `json:"created_at"`
	Status    string `json:"status"`
	Ports     string `json:"ports"`
	State     string `json:"state"`
}

func (a *API) dockerContainers(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}

	// 可用性检测
	verOut, err := s.Run(dockerCheckCmd)
	if err != nil {
		fail(c, 400, "Docker 检测失败: "+err.Error())
		return
	}
	if strings.TrimSpace(verOut) == "" {
		c.JSON(200, gin.H{"available": false, "containers": []any{}})
		return
	}

	out, err := s.Run(dockerListCmd)
	if err != nil && out == "" {
		fail(c, 400, "获取容器列表失败: "+err.Error())
		return
	}

	var containers []dockerContainer
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var raw map[string]any
		if json.Unmarshal([]byte(line), &raw) != nil {
			continue
		}
		dc := dockerContainer{}
		if v, ok := raw["ID"].(string); ok {
			dc.ID = v
			if len(v) > 12 {
				dc.ShortID = v[:12]
			} else {
				dc.ShortID = v
			}
		}
		if v, ok := raw["Names"].(string); ok {
			dc.Name = v
		}
		if v, ok := raw["Image"].(string); ok {
			dc.Image = v
		}
		if v, ok := raw["Command"].(string); ok {
			dc.Command = v
		}
		if v, ok := raw["CreatedAt"].(string); ok {
			// docker 的 CreatedAt 形如 "2024-01-15 10:30:00 +0800 CST"
			// 转成 RFC3339 标准时区格式,前端 new Date() 可正确解析
			dc.CreatedAt = parseDockerTime(v)
		}
		if v, ok := raw["Status"].(string); ok {
			dc.Status = v
		}
		if v, ok := raw["Ports"].(string); ok {
			dc.Ports = v
		}
		if v, ok := raw["State"].(string); ok {
			dc.State = v
		}
		containers = append(containers, dc)
	}
	if containers == nil {
		containers = []dockerContainer{}
	}
	c.JSON(200, gin.H{"available": true, "containers": containers})
}

func (a *API) dockerInspect(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	target := c.Query("cid")
	if target == "" {
		fail(c, 400, "容器 ID 不能为空")
		return
	}
	// 与 logs/exec 一致:cid 白名单清洗 + shellQuote,防拼接注入
	out, err := s.Run(fmt.Sprintf("docker inspect -- %s 2>/dev/null", shellQuote(sanitizeDockerID(target))))
	if err != nil && out == "" {
		fail(c, 400, "docker inspect 失败: "+err.Error())
		return
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" || trimmed == "[]" {
		fail(c, 404, "容器不存在: "+target)
		return
	}
	var parsed any
	if json.Unmarshal([]byte(trimmed), &parsed) != nil {
		fail(c, 400, "docker inspect 返回非 JSON 数据")
		return
	}
	c.Data(200, "application/json", []byte(trimmed))
}

// ---------- Docker 面板信息卡片(版本 + Compose 项目) ----------

// dockerInfoScript 单次 exec 输出标记分段的结果(与 k8smgr.ProbeScript /
// handler/k8s.go 的 wsrun 协议同构):INFO 段为 docker info 全量 JSON(存储
// 驱动与容器/镜像计数),VER 段为 docker version 全量 JSON(Client/Server
// 版本与 API 版本——docker info 不含 APIVersion);compose 优先 docker
// compose(V2 插件),回退 docker-compose(V1 独立二进制),对应 CV2/CV1
// 段报版本号,CLS 段报 compose ls --format json 的项目列表。
const dockerInfoScript = `emit() {
  if [ "$2" -eq 0 ]; then
    printf '__WS_%s_OK__\n%s\n__WS_END__\n' "$1" "$out"
  else
    printf '__WS_%s_ERR__\n%s\n__WS_END__\n' "$1" "$out"
  fi
}
out=$(docker info --format '{{json .}}' 2>&1); emit INFO $?
out=$(docker version --format '{{json .}}' 2>&1); emit VER $?
if docker compose version >/dev/null 2>&1; then
  out=$(docker compose version --short 2>&1); emit CV2 $?
  out=$(docker compose ls --format json 2>/dev/null); emit CLS $?
elif command -v docker-compose >/dev/null 2>&1; then
  out=$(docker-compose version --short 2>&1); emit CV1 $?
  out=$(docker-compose ls --format json 2>/dev/null); emit CLS $?
else
  out="docker compose 与 docker-compose 均未安装"; emit CVER 1
fi`

// dockerServerInfo docker 面板信息卡片的字段(JSON 契约;版本字段来自
// docker version,计数/驱动来自 docker info)。
type dockerServerInfo struct {
	ClientVersion string `json:"client_version,omitempty"`
	ServerVersion string `json:"server_version,omitempty"`
	APIVersion    string `json:"api_version,omitempty"`
	StorageDriver string `json:"storage_driver,omitempty"`
	OSType        string `json:"os_type,omitempty"`
	Architecture  string `json:"arch,omitempty"`
	OS            string `json:"os,omitempty"`
	Containers    int    `json:"containers"`
	Running       int    `json:"running"`
	Paused        int    `json:"paused"`
	Stopped       int    `json:"stopped"`
	Images        int    `json:"images"`
}

// dockerInfoRaw docker info --format '{{json .}}' 的原始键名(与
// dockerServerInfo 的 snake_case 输出契约分开,JSON 反序列化匹配不忽略
// 下划线,共用会全部落空)。存储驱动在 API JSON 里是 Driver(StorageDriver
// 只是 CLI 表格列名,保留兜底)。
type dockerInfoRaw struct {
	ServerVersion     string `json:"ServerVersion"`
	Driver            string `json:"Driver"`
	StorageDriver     string `json:"StorageDriver"`
	OSType            string `json:"OSType"`
	Architecture      string `json:"Architecture"`
	OperatingSystem   string `json:"OperatingSystem"`
	Containers        int    `json:"Containers"`
	ContainersRunning int    `json:"ContainersRunning"`
	ContainersPaused  int    `json:"ContainersPaused"`
	ContainersStopped int    `json:"ContainersStopped"`
	Images            int    `json:"Images"`
}

// dockerVersionRaw docker version --format '{{json .}}' 中版本卡片用到的
// 字段子集(ApiVersion 即远端输出的原始键)。
type dockerVersionRaw struct {
	Client struct {
		Version    string `json:"Version"`
		APIVersion string `json:"ApiVersion"`
		Os         string `json:"Os"`
		Arch       string `json:"Arch"`
	} `json:"Client"`
	Server struct {
		Version    string `json:"Version"`
		APIVersion string `json:"ApiVersion"`
	} `json:"Server"`
}

// dockerComposeProjectRaw compose ls --format json 的原始键名(V1/V2 一致)。
type dockerComposeProjectRaw struct {
	Name        string `json:"Name"`
	Status      string `json:"Status"`
	ConfigFiles string `json:"ConfigFiles"`
}

// dockerComposeProject compose ls 项目列表的单行(name/status 兼容 V1/V2 的
// json 输出键;config_files 供前端悬浮提示)。
type dockerComposeProject struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	ConfigFiles string `json:"config_files"`
}

// dockerComposeInfo Compose 卡片数据;Command 记录实际生效的命令变体。
type dockerComposeInfo struct {
	Command  string                 `json:"command"`           // docker compose | docker-compose,未安装为空
	Version  string                 `json:"version,omitempty"` // --short 输出,如 v2.24.5 / 1.29.2
	Projects []dockerComposeProject `json:"projects"`
}

// parseDockerInfoScript 解析 dockerInfoScript 的分段输出,返回 info 可用性、
// 卡片数据与失败原因;独立成纯函数便于单测注入样例输出。
func parseDockerInfoScript(out string) (info dockerServerInfo, infoOK bool, compose dockerComposeInfo, infoErr string) {
	compose.Projects = []dockerComposeProject{}
	var rawInfo dockerInfoRaw
	var rawVer dockerVersionRaw
	verOK := false
	for _, sec := range k8sSectionRe.FindAllStringSubmatch(out, -1) {
		name, status, body := sec[1], sec[2], strings.TrimSpace(sec[3])
		switch name {
		case "INFO":
			var raw dockerInfoRaw
			if status == "OK" && json.Unmarshal([]byte(body), &raw) == nil && raw.ServerVersion != "" {
				rawInfo, infoOK = raw, true
			} else {
				infoErr = firstLine(body)
			}
		case "VER":
			var raw dockerVersionRaw
			if status == "OK" && json.Unmarshal([]byte(body), &raw) == nil {
				rawVer, verOK = raw, true
			}
		case "CV2":
			compose.Command = "docker compose"
			if status == "OK" {
				compose.Version = body
			}
		case "CV1":
			compose.Command = "docker-compose"
			if status == "OK" {
				compose.Version = body
			}
		case "CLS":
			if status == "OK" {
				compose.Projects = parseComposeProjects(body)
			}
		}
	}
	if infoOK {
		driver := rawInfo.Driver
		if driver == "" {
			driver = rawInfo.StorageDriver
		}
		info = dockerServerInfo{
			ServerVersion: rawInfo.ServerVersion,
			StorageDriver: driver,
			OSType:        rawInfo.OSType,
			Architecture:  rawInfo.Architecture,
			OS:            rawInfo.OperatingSystem,
			Containers:    rawInfo.Containers,
			Running:       rawInfo.ContainersRunning,
			Paused:        rawInfo.ContainersPaused,
			Stopped:       rawInfo.ContainersStopped,
			Images:        rawInfo.Images,
		}
		// VER 段的 Client/Server 版本与 API 版本更权威;失败时退回 info 的 ServerVersion
		if verOK {
			info.ClientVersion = rawVer.Client.Version
			if rawVer.Server.Version != "" {
				info.ServerVersion = rawVer.Server.Version
			}
			info.APIVersion = rawVer.Server.APIVersion
		}
	}
	return info, infoOK, compose, infoErr
}

// parseComposeProjects 解析 compose ls --format json;旧版 compose 不支持
// --format json 时输出表格文本,解析失败退化为空列表(版本号仍可展示)。
func parseComposeProjects(body string) []dockerComposeProject {
	projects := []dockerComposeProject{}
	if body == "" {
		return projects
	}
	var raw []dockerComposeProjectRaw
	if json.Unmarshal([]byte(body), &raw) != nil {
		return projects
	}
	for _, r := range raw {
		projects = append(projects, dockerComposeProject{
			Name:        r.Name,
			Status:      r.Status,
			ConfigFiles: r.ConfigFiles,
		})
	}
	return projects
}

// dockerInfo GET /api/sessions/:id/docker/info
// Docker 版本卡片 + Compose 卡片数据;信息低频变化,仅在打开面板与手动刷新
// 时调用,不进容器列表的 5s 自动轮询。
func (a *API) dockerInfo(c *gin.Context) {
	s, ok := a.mgr.Get(c.Param("id"))
	if !ok {
		fail(c, 404, "会话不存在或已断开")
		return
	}
	out, _ := s.Run(dockerInfoScript)
	info, infoOK, compose, infoErr := parseDockerInfoScript(out)
	if !infoOK {
		c.JSON(200, gin.H{"available": false, "error": infoErr})
		return
	}
	c.JSON(200, gin.H{"available": true, "info": info, "compose": compose})
}

// parseDockerTime 将 docker ps 的 CreatedAt 格式(如 "2024-01-15 10:30:00 +0800 CST")
// 转为 RFC3339 标准格式(如 "2024-01-15T10:30:00+08:00"),前端 new Date() 可正确解析。
func parseDockerTime(s string) string {
	s = strings.TrimSpace(s)
	// 去掉结尾的时区缩写(CST/UTC/EDT 等):time.Parse 的 -0700 格式不识别缩写
	if fields := strings.Fields(s); len(fields) == 4 && isZoneAbbr(fields[3]) {
		s = strings.Join(fields[:3], " ")
	}
	// 尝试解析 "2006-01-02 15:04:05 -0700" 格式
	t, err := time.Parse("2006-01-02 15:04:05 -0700", s)
	if err == nil {
		return t.Format(time.RFC3339)
	}
	// 尝试解析 "2006-01-02 15:04:05" (无时区,用本地)
	t, err = time.ParseInLocation("2006-01-02 15:04:05", s, time.Local)
	if err == nil {
		return t.Format(time.RFC3339)
	}
	return s
}

// isZoneAbbr 判断是否为 2~5 位大写字母的时区缩写。
func isZoneAbbr(s string) bool {
	if len(s) < 2 || len(s) > 5 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}
