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
	handler := httpapi.NewHandler(ingestSvc, askSvc, aiClient, cfg.Server.MaxUploadSizeMB)
	router := httpapi.NewRouter(handler)

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

	<-quit
	logger.Info("shutdown signal received, graceful shutdown...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", "err", err)
	}
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
