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
