// Command server 是 ControlHub 面板的 Go 后端入口。
// 与 PHP 后端并存期监听独立端口；前端/MySQL/API 契约不变。
package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"controlhub-server/internal/collector"
	"controlhub-server/internal/config"
	"controlhub-server/internal/handler/admin"
	"controlhub-server/internal/httpx"
	"controlhub-server/internal/store"
	"controlhub-server/internal/task"
	"controlhub-server/internal/ws"

	"github.com/gin-gonic/gin"
)

func main() {
	app, db, err := bootstrap()
	if err != nil {
		log.Fatalf("启动失败: %v", err)
	}
	if db != nil {
		defer db.Close()
	}

	addr := envOr("LISTEN_ADDR", ":8080")
	router, wsServer := newRouter(app)
	srv := &http.Server{
		Handler:           router,
		Addr:              addr,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// 优雅退出：收到 SIGINT/SIGTERM 后停止接新连接，给在途请求 15 秒收尾。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 启动调度器（P9 常驻 goroutine，随服务启动/优雅退出一起关闭）
	startScheduler(ctx, app, db)

	// 启动节点 WS 通道（轮询/心跳节拍；握手由 /node-ws 路由 ServeHTTP 处理）
	if wsServer != nil {
		go wsServer.Run(ctx)
	}

	go func() {
		log.Printf("ControlHub Go 后端启动，监听 %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("服务启动失败: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("收到退出信号，正在关闭服务…")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("服务关闭异常: %v", err)
	}
}

// bootstrap 加载配置并连接数据库。
// CONFIG_ENV 指定 .env 路径，缺省复用 backend/.env（含 APP_KEY 与 DB_*）。
func bootstrap() (*httpx.App, *sql.DB, error) {
	cfg, err := config.Load(envOr("CONFIG_ENV", config.DefaultEnvPath))
	if err != nil {
		return nil, nil, err
	}

	db, err := store.Open(cfg.DB)
	if err != nil {
		return nil, nil, err
	}

	return httpx.NewApp(db, cfg), db, nil
}

// newRouter 构建 gin 引擎并注册路由。单独拆出来便于单测/健康检查复用。
// 返回 gin 引擎 + 节点 WS 服务（WS 的 Run 需由 main 用 ctx 启动）。
func newRouter(app *httpx.App) (*gin.Engine, *ws.Server) {
	if app.Cfg.AppEnv == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	// 路由注册随迁移阶段逐步接入（handler/api、handler/admin）。
	// /api/ping 由 api.Handler.Register 注册（对齐 PHP 契约）。
	wsServer := registerRoutes(r, app)

	return r, wsServer
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// startScheduler 组装并启动定时调度器（P9 常驻 goroutine）。
// 队列分离默认关闭（全部进 default 队列）。
func startScheduler(ctx context.Context, app *httpx.App, db *sql.DB) {
	logger := log.Default()
	cfg := app.Cfg

	// collector（P6 采集层）
	col, colErr := collector.New(collector.Options{
		DB:     db,
		AppKey: cfg.AppKey,
		Now:    time.Now,
		Logf:   logger.Printf,
	})
	if colErr != nil {
		logger.Printf("[schedule] 初始化 collector 失败: %v", colErr)
		return
	}

	// 异步任务服务（async-task-timeout 用）
	asyncSvc := task.NewService(db, nil)

	// 节点 store（BumpConfigVersion 复用 Node::bumpXrayConfigVersion 语义）
	nodeStore := admin.NewNodeSQLStore(db, cfg.AppKey)

	// Geo 数据服务（geodata:update 命令用）
	geoService := admin.NewGeodataService(db, envOr("GEODATA_DIR", filepath.Join("storage", "app", "xray", "geo")), nil)

	// 命令运行器（13 个 artisan 命令）
	cmdRunner := task.NewCommandRunner(task.CommandDeps{
		DB:         db,
		AppKey:     cfg.AppKey,
		Logf:       logger.Printf,
		Now:        time.Now,
		BackupBin:  envOr("BACKUP_MYSQLDUMP_BIN", "mysqldump"),
		BackupDir:  envOr("BACKUP_DIR", "/var/backups/3xui-hub"),
		BackupKeep: 7,
		DBHost:     cfg.DB.Host,
		DBPort:     cfg.DB.Port,
		DBName:     cfg.DB.Database,
		DBUser:     cfg.DB.Username,
		DBPassword: cfg.DB.Password,

		BumpConfigVersion: nodeStore.BumpConfigVersion,
		Geodata:           &geodataAdapter{inner: geoService},
	})

	// 调度器
	sched := task.NewScheduler(task.Options{
		Now:  time.Now,
		Logf: logger.Printf,
	})

	if err := sched.Register(task.DefaultJobs(task.Deps{
		Collector: col,
		Tasks:     asyncSvc,
		Commands:  cmdRunner,
		Lookups:   &task.SQLLookups{DB: db},
		Logf:      logger.Printf,
	})...); err != nil {
		logger.Printf("[schedule] 注册调度任务失败: %v", err)
		return
	}

	sched.Start(ctx)
	logger.Printf("[schedule] 调度器已启动（%d 条任务）", len(sched.Entries()))
}
