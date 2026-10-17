package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"paper-rag-backend/internal/ai"
	"paper-rag-backend/internal/config"
	"paper-rag-backend/internal/httpapi"
	"paper-rag-backend/internal/logger"
	"paper-rag-backend/internal/render"
	"paper-rag-backend/internal/service"
	"paper-rag-backend/internal/store"
	"paper-rag-backend/internal/worker"
)

func main() {
	configPath := flag.String("config", "config/config.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config failed: %v", err)
	}

	// 日志级别在配置加载完成后立即生效（加载阶段的错误仍走标准库 log）
	logger.Init(cfg.Log.Level)
	logger.Info("config loaded", "path", *configPath, "log_level", cfg.Log.Level)

	storeImpl, err := buildStore(cfg)
	if err != nil {
		logger.Error("failed to init store", "err", err)
		os.Exit(1)
	}
	if closer, ok := storeImpl.(interface{ Close() }); ok {
		defer closer.Close()
	}

	// 组件装配
	aiClient := ai.NewClient(cfg.AI)
	renderCfg := render.Config{
		PythonCmd: cfg.Render.PythonCmd,
		DPI:       cfg.Render.DPI,
		WorkDir:   cfg.Render.WorkDir,
	}
	ingestSvc := service.NewIngestService(aiClient, storeImpl, cfg.Server, cfg.RAG, renderCfg)
	askSvc := service.NewAskService(aiClient, storeImpl, cfg.RAG)
	searchSvc := service.NewSearchService(aiClient, storeImpl, cfg.RAG)

	// 异步摄入工作池：DB（documents.status）当队列，channel 只当门铃。
	// 上传接口 Accept 入队后调 pool.Kick 唤醒 worker 立刻抢单。
	ingestPool := worker.NewPool(ingestSvc, storeImpl, cfg.RAG.IngestWorkers)
	handler := httpapi.NewHandler(ingestSvc, askSvc, searchSvc, aiClient, cfg.Server.MaxUploadSizeMB, ingestPool.Kick, cfg.Server.AdminUsers)
	router := httpapi.NewRouter(handler, cfg.Server.APIKey)

	srv := &http.Server{
		Addr:         cfg.Addr(),
		Handler:      router,
		ReadTimeout:  time.Duration(cfg.Server.ReadTimeoutSec) * time.Second,
		WriteTimeout: time.Duration(cfg.Server.WriteTimeoutSec) * time.Second,
	}

	// 优雅退出
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		logger.Info("RAG service started", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed to start", "err", err)
			os.Exit(1)
		}
	}()

	// 摄入 worker：先恢复（上次硬崩的 processing 悬案重置回 pending），再起池子
	if n, err := ingestPool.Recover(); err != nil {
		logger.Error("worker recover failed", "err", err)
		os.Exit(1)
	} else if n > 0 {
		logger.Info("worker recover done", "reset_to_pending", n)
	}
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	ingestPool.Start(workerCtx)

	<-quit
	logger.Info("shutdown signal received, graceful shutdown...")

	// 先停 HTTP（不再接收新上传），再停 worker（停抢单 → 宽限在途 → 超时重置回 pending）
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", "err", err)
	}
	workerCancel()                         // worker 停止抢单
	ingestPool.Drain(30 * time.Second)     // 宽限等在途任务；超时重置由下次启动续跑
	logger.Info("server exited")
}

// buildStore 按配置构建存储实现（PostgreSQL + pgvector）。
func buildStore(cfg *config.Config) (store.Store, error) {
	switch cfg.Database.Type {
	case "postgres", "":
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return store.NewPostgresStore(ctx, cfg.Database.DSN(), cfg.Database.MaxConns)
	default:
		return nil, fmt.Errorf("未知的存储类型: %q（当前仅支持: postgres）", cfg.Database.Type)
	}
}
