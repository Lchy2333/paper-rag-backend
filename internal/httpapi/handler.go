package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"paper-rag-backend/internal/model"
	"paper-rag-backend/internal/service"
)

// documentService 是 Handler 对文档服务的依赖抽象，由 main 注入 *service.IngestService。
// 抽象成接口便于对 HTTP 层做单测（用假实现替换真实服务）。
type documentService interface {
	Process(ctx context.Context, file io.ReaderAt, size int64, filename string) (*model.Document, error)
	Get(ctx context.Context, id string) (*model.Document, error)
	List(ctx context.Context) ([]model.Document, error)
	Delete(ctx context.Context, id string) error
}

// Handler 聚合所有 HTTP 处理器依赖。
type Handler struct {
	ingest         documentService
	ask            *service.AskService
	maxUploadBytes int64
}

// NewHandler 创建处理器。
func NewHandler(ingest documentService, ask *service.AskService, maxUploadMB int64) *Handler {
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

// UploadDocument 上传并摄入一个或多个 PDF 文档。
// 逐个处理、互不影响：单个文件失败只标记该条结果，不拖累其他文件。
// 统一返回 200 + 逐条带状态的 results，便于批量查看。
func (h *Handler) UploadDocument(c *gin.Context) {
	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "解析上传表单失败: " + err.Error()})
		return
	}
	files := form.File["file"]
	if len(files) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少文件字段 file"})
		return
	}

	results := make([]model.UploadResult, 0, len(files))
	success, failed, duplicates := 0, 0, 0
	maxMB := h.maxUploadBytes / (1024 * 1024)

	for _, fh := range files {
		r := model.UploadResult{Filename: fh.Filename, SizeBytes: fh.Size}

		if fh.Size > h.maxUploadBytes {
			r.Error = fmt.Sprintf("文件过大（超过 %d MB）", maxMB)
			failed++
			results = append(results, r)
			continue
		}

		start := time.Now()
		f, err := fh.Open()
		if err != nil {
			r.Error = fmt.Sprintf("读取上传文件失败: %v", err)
			failed++
			results = append(results, r)
			continue
		}
		doc, perr := h.ingest.Process(c.Request.Context(), f, fh.Size, fh.Filename)
		_ = f.Close()
		r.DurationMS = time.Since(start).Milliseconds()

		if doc != nil {
			r.DocumentID = doc.ID
			r.PageCount = doc.PageCount
		}
		if perr != nil {
			if errors.Is(perr, service.ErrDuplicateDocument) {
				r.Duplicate = true
				r.Error = perr.Error()
				duplicates++
			} else {
				r.Error = perr.Error()
				failed++
			}
		} else {
			r.Success = true
			success++
		}
		results = append(results, r)
	}

	c.JSON(http.StatusOK, gin.H{
		"total":           len(results),
		"success_count":   success,
		"failed_count":    failed,
		"duplicate_count": duplicates,
		"results":         results,
	})
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
