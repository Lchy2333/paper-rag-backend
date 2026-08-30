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
	"paper-rag-backend/internal/service"
	"paper-rag-backend/internal/store"
)

func main() {
	configPath := flag.String("config", "config/config.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	storeImpl, err := buildStore(cfg)
	if err != nil {
		log.Fatalf("初始化存储失败: %v", err)
	}
	if closer, ok := storeImpl.(interface{ Close() }); ok {
		defer closer.Close()
	}

	// 组件装配
	aiClient := ai.NewClient(cfg.AI)
	ingestSvc := service.NewIngestService(aiClient, storeImpl, cfg.Server, cfg.RAG)
	askSvc := service.NewAskService(aiClient, storeImpl, cfg.RAG)
	handler := httpapi.NewHandler(ingestSvc, askSvc, cfg.Server.MaxUploadSizeMB)
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
		log.Printf("RAG 服务启动，监听 %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("服务启动失败: %v", err)
		}
	}()

	<-quit
	log.Println("收到退出信号，正在优雅关闭...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("关闭服务出错: %v", err)
	}
	log.Println("服务已退出")
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
