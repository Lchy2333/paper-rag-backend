package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"paper-rag-backend/internal/model"
	"paper-rag-backend/internal/service"
)

// Handler 聚合所有 HTTP 处理器依赖。
type Handler struct {
	ingest         *service.IngestService
	ask            *service.AskService
	maxUploadBytes int64
}

// NewHandler 创建处理器。
func NewHandler(ingest *service.IngestService, ask *service.AskService, maxUploadMB int64) *Handler {
	return &Handler{
		ingest:         ingest,
		ask:            ask,
		maxUploadBytes: maxUploadMB * 1024 * 1024,
	}
}

// Health 健康检查。
func (h *Handler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "time": time.Now().Format(time.RFC3339)})
}

// UploadDocument 上传并摄入 PDF 文档。
func (h *Handler) UploadDocument(c *gin.Context) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少文件字段 file"})
		return
	}
	defer file.Close()

	if header.Size > h.maxUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "文件过大"})
		return
	}

	doc, err := h.ingest.Process(c.Request.Context(), file, header.Size, header.Filename)
	if err != nil {
		if errors.Is(err, service.ErrUnsupportedType) {
			c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": err.Error()})
			return
		}
		// 摄入失败时返回 422，同时附带文档状态便于排查
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error(), "document": doc})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"document": doc})
}

// ListDocuments 列出所有文档。
func (h *Handler) ListDocuments(c *gin.Context) {
	docs, err := h.ingest.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"documents": docs})
}

// GetDocument 查询单个文档详情。
func (h *Handler) GetDocument(c *gin.Context) {
	doc, err := h.ingest.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if doc == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "文档不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"document": doc})
}

// DeleteDocument 删除文档。
func (h *Handler) DeleteDocument(c *gin.Context) {
	if err := h.ingest.Delete(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": c.Param("id")})
}

// Ask 基于已入库文档回答用户问题（RAG）。
func (h *Handler) Ask(c *gin.Context) {
	var q model.Question
	if err := c.ShouldBindJSON(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误: " + err.Error()})
		return
	}
	answer, err := h.ask.Ask(c.Request.Context(), q)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"answer": answer})
}
