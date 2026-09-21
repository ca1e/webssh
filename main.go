package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"webssh/internal/auth"
	"webssh/internal/config"
	"webssh/internal/handler"
	"webssh/internal/mcp"
	"webssh/internal/secrets"
	"webssh/internal/sshmgr"
	"webssh/internal/store"
	"webssh/internal/transfer"
)

//go:embed all:frontend/dist
var distFS embed.FS

func main() {
	cfg := config.Load()
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		log.Fatalf("创建配置目录 %s 失败: %v", cfg.ConfigDir, err)
	}
	// 凭据库先于主机配置打开:store 的 legacy 迁移要把旧密码写入 secrets
	// (secrets 库先于主机配置初始化的顺序依赖);secrets 打开失败直接 fatal,
	// 不带病迁移。
	sec, err := secrets.Open(cfg.ConfigDir)
	if err != nil {
		log.Fatalf("打开凭据库失败: %v", err)
	}
	st, err := store.Open(cfg.ConfigDir, sec)
	if err != nil {
		log.Fatalf("加载主机配置失败: %v", err)
	}
	authSt, err := auth.Open(cfg.ConfigDir)
	if err != nil {
		log.Fatalf("加载启动密码配置失败: %v", err)
	}
	mgr := sshmgr.New(cfg.ConfigDir, cfg.MaxSessions, cfg.OrphanGrace)
	mgr.SetSecretResolver(sec.Reveal)
	transfers := transfer.NewManager(func(sid string) (transfer.Side, string, error) {
		s, ok := mgr.Get(sid)
		if !ok {
			return nil, "", fmt.Errorf("会话不存在或已断开")
		}
		sc, err := s.SFTP()
		if err != nil {
			return nil, "", err
		}
		return transfer.NewRemoteSide(sc, s.Name()), s.Name(), nil
	})

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	// MCP:/mcp 装配。认证口令按优先级:WEBSSH_MCP_TOKEN > mcp.json token >
	// 锁屏密码(不另行配置)。中间件逐请求 fail-closed:三处口令均不存在或
	// WEBSSH_MCP_ENABLED=false 时 /mcp 一律 404(不探测不暴露服务存在性);
	// 锁屏密码后设/更换立即生效,无需重启。配置非法仍启动 fatal。
	mcpsrv, err := mcp.New(cfg.ConfigDir, st, mgr, authSt)
	if err != nil {
		log.Fatalf("MCP 配置加载失败: %v", err)
	}
	mcpsrv.Register(r)
	log.Printf("MCP 就绪:/mcp(Bearer 认证,口令来源:%s)", mcpsrv.TokenSourceName())

	handler.NewAPI(cfg, st, mgr, transfers, authSt, mcpsrv).Register(r)
	// SIGHUP 热重载 mcp.json:失败保持旧策略 + 告警日志。
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if err := mcpsrv.Reload(); err != nil {
				log.Printf("mcp.json 热重载失败,保持旧策略: %v", err)
			} else {
				log.Printf("mcp.json 已热重载")
			}
		}
	}()

	dist, err := fs.Sub(distFS, "frontend/dist")
	if err != nil {
		log.Fatalf("嵌入前端资源失败: %v", err)
	}
	indexData, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		log.Fatal("前端资源缺失:请先执行 make frontend-build")
	}
	// SPA 入口禁缓存:升级二进制后浏览器必须拿到新 index.html(其中的
	// hash 资产名才指向新版本);hash 资产名不变的内容由浏览器自行缓存。
	serveIndex := func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache")
		c.Data(http.StatusOK, "text/html; charset=utf-8", indexData)
	}
	fileServer := http.FileServerFS(dist)

	r.GET("/", serveIndex)
	r.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		if strings.HasPrefix(p, "/api/") {
			c.JSON(404, gin.H{"error": "接口不存在"})
			return
		}
		if p == "/index.html" {
			serveIndex(c)
			return
		}
		if _, err := fs.Stat(dist, strings.TrimPrefix(p, "/")); err == nil {
			fileServer.ServeHTTP(c.Writer, c.Request)
			return
		}
		serveIndex(c)
	})

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: r}

	// SIGINT/SIGTERM 优雅退出:先停 HTTP 接入,再取消传输任务并等待收尾,
	// 最后断开全部会话(含端口转发),避免进行中的传输与监听被硬杀。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Printf("收到退出信号,正在关闭…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP 服务关闭异常: %v", err)
		}
		transfers.CancelAll()
		if !transfers.WaitIdle(8 * time.Second) {
			log.Printf("部分传输任务未能在限期内收尾,强制断开")
		}
		mgr.CloseAll()
	}()

	log.Printf("webssh 已启动,监听 %s(配置目录 %s)", cfg.ListenAddr, cfg.ConfigDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
