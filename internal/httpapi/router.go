package httpapi

import (
	"github.com/gin-gonic/gin"
)

// NewRouter 注册所有路由并返回 gin 引擎。
func NewRouter(h *Handler) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	api := r.Group("/api/v1")
	{
		api.GET("/health", h.Health)

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
	}

	return r
}
