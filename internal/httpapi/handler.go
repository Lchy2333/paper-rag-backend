package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"paper-rag-backend/internal/logger"
	"paper-rag-backend/internal/model"
	"paper-rag-backend/internal/service"
)

// documentService 是 Handler 对文档服务的依赖抽象，由 main 注入 *service.IngestService。
// 抽象成接口便于对 HTTP 层做单测（用假实现替换真实服务）。
type documentService interface {
	Process(ctx context.Context, file io.ReaderAt, size int64, filename string, opts *model.IngestOptions) (*model.Document, error)
	Rechunk(ctx context.Context, id string, opts *model.IngestOptions) (*model.Document, error)
	Get(ctx context.Context, id string) (*model.Document, error)
	List(ctx context.Context) ([]model.Document, error)
	Delete(ctx context.Context, id string) error
}

// connectionTester 是 LLM 连接测试的依赖抽象（由 *ai.Client 实现）。
type connectionTester interface {
	TestConnection(ctx context.Context, req model.ConnectionTest) *model.ConnectionResult
}

// Handler 聚合所有 HTTP 处理器依赖。
type Handler struct {
	ingest         documentService
	ask            *service.AskService
	tester         connectionTester
	maxUploadBytes int64
}

// NewHandler 创建处理器。
func NewHandler(ingest documentService, ask *service.AskService, tester connectionTester, maxUploadMB int64) *Handler {
	return &Handler{
		ingest:         ingest,
		ask:            ask,
		tester:         tester,
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
// 可选的表单字段（作用于本次上传的全部文件）：
//
//	chunk_size:    覆盖默认分块大小（config.yaml rag.chunk_size）
//	chunk_overlap: 覆盖默认重叠字符数（config.yaml rag.chunk_overlap）
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
	opts, err := parseIngestOptions(form)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
		logger.Debug("upload.file_start", "file", fh.Filename, "size_bytes", fh.Size)
		doc, perr := h.ingest.Process(c.Request.Context(), f, fh.Size, fh.Filename, opts)
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
	logger.Debug("upload.file_done", "file", fh.Filename, "success", r.Success, "duplicate", r.Duplicate,
		"doc_id", r.DocumentID, "duration_ms", r.DurationMS, "err", r.Error)
	results = append(results, r)
}

	logger.Info("upload.done", "total", len(results), "success", success, "failed", failed, "duplicate", duplicates)
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

// parseIngestOptions 从 multipart 表单解析可选的文档级分块参数。
// 字段缺省或为空时对应字段保持 nil（回退默认值）。
func parseIngestOptions(form *multipart.Form) (*model.IngestOptions, error) {
	opts := &model.IngestOptions{}
	parse := func(key string, dst **int) error {
		vals := form.Value[key]
		if len(vals) == 0 || strings.TrimSpace(vals[0]) == "" {
			return nil
		}
		n, err := strconv.Atoi(strings.TrimSpace(vals[0]))
		if err != nil {
			return fmt.Errorf("%s 不是合法整数: %v", key, err)
		}
		*dst = &n
		return nil
	}
	if err := parse("chunk_size", &opts.ChunkSize); err != nil {
		return nil, err
	}
	if err := parse("chunk_overlap", &opts.ChunkOverlap); err != nil {
		return nil, err
	}
	return opts, nil
}

// RechunkDocument 删除文档的全部 chunk 后，用新的分块参数重新分块入库。
// 请求体为可选的 JSON：{"chunk_size": 400, "chunk_overlap": 50}；空 body 用文档当前参数。
func (h *Handler) RechunkDocument(c *gin.Context) {
	opts, err := bindOptionalIngestOptions(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	doc, err := h.ingest.Rechunk(c.Request.Context(), c.Param("id"), opts)
	if err != nil {
		if errors.Is(err, service.ErrDocumentNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"document": doc})
}

// bindOptionalIngestOptions 绑定可选的 JSON 请求体；空 body 返回 nil。
func bindOptionalIngestOptions(c *gin.Context) (*model.IngestOptions, error) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, fmt.Errorf("读取请求体失败: %v", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, nil
	}
	opts := &model.IngestOptions{}
	if err := json.Unmarshal(body, opts); err != nil {
		return nil, fmt.Errorf("请求格式错误: %v", err)
	}
	return opts, nil
}

// TestConnection 向用户提供的 base_url 发一次最小请求，验证 LLM 连通性。
// 请求体：{"kind": "embedding"|"chat"|"formula", "base_url": "...", "api_key": "...", "model": "..."}
// kind: embedding=一次向量化 / chat=一次 "ping" / formula=发一张内置测试图 OCR。
func (h *Handler) TestConnection(c *gin.Context) {
	var req model.ConnectionTest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误: " + err.Error()})
		return
	}
	result := h.tester.TestConnection(c.Request.Context(), req)
	logger.Info("connection.test", "kind", req.Kind, "model", req.Model, "base_url", req.BaseURL, "ok", result.OK, "latency_ms", result.LatencyMS)
	c.JSON(http.StatusOK, result)
}

// GetDocument 查询单个文档详情。

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
		logger.Error("ask.request_failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	logger.Debug("ask.request_done", "query", truncate(q.Query, 120), "hits", answer.TotalChunks, "latency_ms", answer.LatencyMS)
	c.JSON(http.StatusOK, gin.H{"answer": answer})
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}
