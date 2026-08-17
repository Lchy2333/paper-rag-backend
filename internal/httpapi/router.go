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

		// 文档管理
		api.POST("/documents", h.UploadDocument)
		api.GET("/documents", h.ListDocuments)
		api.GET("/documents/:id", h.GetDocument)
		api.DELETE("/documents/:id", h.DeleteDocument)

		// RAG 问答
		api.POST("/ask", h.Ask)
	}

	return r
}
