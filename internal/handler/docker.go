package handler

// Docker 管理(仅查看):脚本常量与解析器统一收纳在 internal/opsview
// (Web 与 MCP 共用同一套,禁止复制第二套解析器);本文件仅保留 Web
// handler 与包内别名。

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"webssh/internal/opsview"
)

// ---------- 与 opsview 的包内别名 ----------

var dockerCheckCmd = opsview.DockerCheckCmd
var dockerListCmd = opsview.DockerListCmd
var dockerInfoScript = opsview.DockerInfoScript

type dockerContainer = opsview.Container
type dockerServerInfo = opsview.ServerInfo
type dockerComposeProject = opsview.ComposeProject
type dockerComposeInfo = opsview.ComposeInfo

func parseDockerInfoScript(out string) (info dockerServerInfo, infoOK bool, compose dockerComposeInfo, infoErr string) {
	return opsview.ParseDockerInfoScript(out)
}
func parseComposeProjects(body string) []dockerComposeProject {
	return opsview.ParseComposeProjects(body)
}
func parseDockerTime(s string) string { return opsview.ParseDockerTime(s) }

// ---------- handler ----------

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

	containers := opsview.ParseContainerList(out)
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
