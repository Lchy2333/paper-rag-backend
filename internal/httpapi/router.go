package httpapi

import (
	"fmt"

	"github.com/gin-gonic/gin"
)

// accessLogFormatter 统一访问日志格式（与 paper-rag-agent 侧对齐）：
//
//	[ACCESS] 2026/10/25 - 06:17:40 | 200 | 17.851µs | 172.18.0.4 | GET "/api/v1/health"
func accessLogFormatter(param gin.LogFormatterParams) string {
	return fmt.Sprintf("[ACCESS] %s | %d | %s | %s | %s %q\n",
		param.TimeStamp.Format("2006/01/02 - 15:04:05"),
		param.StatusCode,
		param.Latency,
		param.ClientIP,
		param.Method,
		param.Path,
	)
}

// NewRouter 注册所有路由并返回 gin 引擎。
//
// apiKey 是服务间鉴权密钥（Agent 调用时带 X-API-Key 请求头）：
// 为空则不启用鉴权（本地开发模式），非空则除 /health 外全部接口须持有效 key。
func NewRouter(h *Handler, apiKey string) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.LoggerWithFormatter(accessLogFormatter), gin.Recovery())

	// 健康检查不鉴权：不泄露业务数据，且 docker-compose healthcheck 无需配置 key
	r.GET("/api/v1/health", h.Health)

	api := r.Group("/api/v1")
	api.Use(APIKeyAuth(apiKey), UserContext())
	{
		// LLM 连接测试（用户手动验证自己的模型配置是否可用）
		api.POST("/connections/test", h.TestConnection)

		// 文档管理
		api.POST("/documents", h.UploadDocument)
		api.GET("/documents", h.ListDocuments)
		api.GET("/documents/:id", h.GetDocument)
		api.POST("/documents/:id/rechunk", h.RechunkDocument)
		api.DELETE("/documents/:id", h.DeleteDocument)

		// RAG 问答
		api.POST("/ask", h.Ask)

		// RAG 纯检索（不调 LLM，Agent 默认入口；与 /ask 保留并行）
		api.POST("/search", h.Search)
	}

	return r
}
