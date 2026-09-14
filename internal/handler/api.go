package handler

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"webssh/internal/config"
	"webssh/internal/sshmgr"
	"webssh/internal/store"
	"webssh/internal/transfer"
)

type API struct {
	cfg *config.Config
	st  *store.Store
	mgr *sshmgr.Manager
	tr  *transfer.Manager
}

func NewAPI(cfg *config.Config, st *store.Store, mgr *sshmgr.Manager, tr *transfer.Manager) *API {
	return &API{cfg: cfg, st: st, mgr: mgr, tr: tr}
}

func fail(c *gin.Context, code int, msg string) {
	c.JSON(code, gin.H{"error": msg})
}

// Register 注册全部路由;各域 handler 分布在同包的
// hosts.go / sessions.go / terminal.go / sftp.go / transfer.go / local.go /
// forwards.go / relay.go / docker.go / docker_ws.go / metrics.go 中。
func (a *API) Register(r gin.IRouter) {
	r.GET("/api/health", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) })

	// 主机管理
	r.GET("/api/hosts", a.listHosts)
	r.POST("/api/hosts", a.createHost)
	r.PUT("/api/hosts/:id", a.updateHost)
	r.DELETE("/api/hosts/:id", a.deleteHost)
	r.POST("/api/hosts/:id/duplicate", a.duplicateHost)
	r.POST("/api/hosts/test", a.testHost)
	r.GET("/api/keys", a.listKeys)

	// 会话
	r.GET("/api/sessions", a.listSessions)
	r.POST("/api/sessions", a.openSession)
	r.DELETE("/api/sessions/:id", a.closeSession)
	r.GET("/api/sessions/:id/ws", a.terminalWS)
	r.GET("/api/sessions/:id/cwd", a.sessionCwd)

	// 远程 SFTP
	r.GET("/api/sessions/:id/sftp/ls", a.sftpList)
	r.DELETE("/api/sessions/:id/sftp", a.sftpDelete)
	r.POST("/api/sessions/:id/sftp/mkdir", a.sftpMkdir)
	r.POST("/api/sessions/:id/sftp/rename", a.sftpRename)

	// 本机文件
	r.GET("/api/local/ls", a.localList)
	r.DELETE("/api/local", a.localDelete)
	r.POST("/api/local/mkdir", a.localMkdir)
	r.POST("/api/local/rename", a.localRename)

	// 端口转发 / SOCKS5
	r.GET("/api/sessions/:id/forwards", a.listForwards)
	r.POST("/api/sessions/:id/forwards", a.addForward)
	r.DELETE("/api/sessions/:id/forwards/:fid", a.delForward)

	// 统一文件传输(本机↔远程、远程↔远程,异步任务 + WS 进度推送)
	r.POST("/api/transfers", a.createTransfer)
	r.GET("/api/transfers", a.listTransfers)
	r.GET("/api/transfers/ws", a.transfersWS)
	r.POST("/api/transfers/:id/cancel", a.cancelTransfer)
	r.DELETE("/api/transfers/:id", a.removeTransfer)

	// 主机详情 + Docker 管理
	r.GET("/api/sessions/:id/host-info", a.hostInfo)
	r.GET("/api/sessions/:id/metrics/ws", a.metricsWS)
	r.GET("/api/sessions/:id/docker/containers", a.dockerContainers)
	r.GET("/api/sessions/:id/docker/inspect", a.dockerInspect)
	r.GET("/api/sessions/:id/docker/logs/ws", a.dockerLogsWS)
	r.GET("/api/sessions/:id/docker/shell/ws", a.dockerExecWS)
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

// ---------- 文件操作公共校验(sftp.go 与 local.go 共用) ----------

// validName 校验目录/文件名:非空、≤64 字符、不含路径分隔符与控制字符。
func validName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("名称不能为空")
	}
	if utf8.RuneCountInString(name) > 64 {
		return fmt.Errorf("名称长度不能超过 64 个字符")
	}
	if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return fmt.Errorf("名称不能包含 / 或 \\,且不能为 . 或 ..")
	}
	for _, r := range name {
		if r < 32 {
			return fmt.Errorf("名称不能包含控制字符")
		}
	}
	return nil
}

type mkdirReq struct {
	Path string `json:"path"`
}

type renameReq struct {
	Path    string `json:"path"`
	NewName string `json:"new_name"`
}
