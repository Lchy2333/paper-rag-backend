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
// userID 由 UserContext 中间件注入（admin 为超级用户，作用域传空串 = 全库）。
// Accept 是摄入的同步段（校验/落盘/去重/建 pending 记录，秒回）；
// 慢活（解析/OCR/向量化）由 worker 异步执行，前端轮询 GET /documents/:id 看 status。
type documentService interface {
	Accept(ctx context.Context, file io.ReaderAt, size int64, filename string, userID string, opts *model.IngestOptions) (*model.Document, error)
	Rechunk(ctx context.Context, id string, userID string, opts *model.IngestOptions) (*model.Document, error)
	Get(ctx context.Context, id string, userID string) (*model.Document, error)
	List(ctx context.Context, userID string) ([]model.Document, error)
	Delete(ctx context.Context, id string, userID string) error
}

// connectionTester 是 LLM 连接测试的依赖抽象（由 *ai.Client 实现）。
type connectionTester interface {
	TestConnection(ctx context.Context, req model.ConnectionTest) *model.ConnectionResult
}

// Handler 聚合所有 HTTP 处理器依赖。
type Handler struct {
	ingest         documentService
	ask            *service.AskService
	search         *service.SearchService
	tester         connectionTester
	maxUploadBytes int64
	// notifyIngest 摄入门铃：Accept 成功入队后唤醒 worker 立刻抢单（worker.Pool.Kick）。
	// 可为 nil（本地无 worker 场景/单测），nil 时不按铃，靠 worker 兜底轮询。
	notifyIngest func()
	// adminUsers 超级用户集合（Agent 的 user_id）：全库检索/管理，ask 仅限其调用。
	adminUsers map[string]struct{}
}

// NewHandler 创建处理器。
func NewHandler(ingest documentService, ask *service.AskService, search *service.SearchService, tester connectionTester, maxUploadMB int64, notifyIngest func(), adminUsers []string) *Handler {
	admins := make(map[string]struct{}, len(adminUsers))
	for _, u := range adminUsers {
		admins[u] = struct{}{}
	}
	return &Handler{
		ingest:         ingest,
		ask:            ask,
		search:         search,
		tester:         tester,
		maxUploadBytes: maxUploadMB * 1024 * 1024,
		notifyIngest:   notifyIngest,
		adminUsers:     admins,
	}
}

// isAdmin 判断 userID 是否为配置的超级用户（config.yaml server.admin_users）。
func (h *Handler) isAdmin(userID string) bool {
	_, ok := h.adminUsers[userID]
	return ok
}

// scopeUserID 把身份转为"数据作用域"：超级用户 → 空串 = 不过滤（全库）。
func (h *Handler) scopeUserID(userID string) string {
	if h.isAdmin(userID) {
		return ""
	}
	return userID
}

// Health 健康检查。
func (h *Handler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "time": time.Now().Format(time.RFC3339)})
}

// scopeUserID 方法已上移；此处为调用方：handler 内统一用 h.scopeUserID / h.isAdmin。

// UploadDocument 上传一个或多个 PDF 文档（**异步摄入语义**，2026-10 起）：
// 只做接收段（校验/落盘/去重/建 pending 记录）秒回，慢活（解析/OCR/向量化）由后台 worker
// 异步执行；前端拿 document_id 轮询 GET /documents/:id 的 status（pending→processing→ready/failed）。
// 逐个接收、互不影响：单个文件失败只标记该条结果，不拖累其他文件。
// 统一返回 200 + 逐条带状态的 results，便于批量查看。
// 可选的表单字段（作用于本次上传的全部文件）：
//
//	chunk_size:    覆盖默认分块大小（config.yaml rag.chunk_size）
//	chunk_overlap: 覆盖默认重叠字符数（config.yaml rag.chunk_overlap）
func (h *Handler) UploadDocument(c *gin.Context) {
	userID := c.GetString("user_id")
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
		doc, perr := h.ingest.Accept(c.Request.Context(), f, fh.Size, fh.Filename, userID, opts)
		_ = f.Close()
		r.DurationMS = time.Since(start).Milliseconds()

		if doc != nil {
			r.DocumentID = doc.ID
			r.Status = doc.Status // 接收成功 = "pending"（异步处理中）；重复 = 已有文档状态
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
			r.Success = true // 语义：已接收并入队（处理进度看文档 status）
			success++
			if h.notifyIngest != nil {
				h.notifyIngest() // 按门铃：唤醒 worker 立刻抢单
			}
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

// ListDocuments 列出文档（普通用户只看自己的；admin 看全部）。
func (h *Handler) ListDocuments(c *gin.Context) {
	userID := h.scopeUserID(c.GetString("user_id"))
	docs, err := h.ingest.List(c.Request.Context(), userID)
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
	userID := h.scopeUserID(c.GetString("user_id"))
	opts, err := bindOptionalIngestOptions(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	doc, err := h.ingest.Rechunk(c.Request.Context(), c.Param("id"), userID, opts)
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

// GetDocument 查询单个文档详情（校验归属；admin 可看任何文档）。
func (h *Handler) GetDocument(c *gin.Context) {
	userID := h.scopeUserID(c.GetString("user_id"))
	doc, err := h.ingest.Get(c.Request.Context(), c.Param("id"), userID)
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

// DeleteDocument 删除文档（校验归属；admin 可删任何文档）。
func (h *Handler) DeleteDocument(c *gin.Context) {
	userID := h.scopeUserID(c.GetString("user_id"))
	if err := h.ingest.Delete(c.Request.Context(), c.Param("id"), userID); err != nil {
		if errors.Is(err, service.ErrDocumentNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": c.Param("id")})
}

// Ask 基于已入库文档回答用户问题（RAG）。
// 安全：ask 是**全局检索**（不按用户过滤），仅管理员可调——它是内部测试接口，
// 不对用户暴露（Agent 侧不提供 ask_rag 工具）。
func (h *Handler) Ask(c *gin.Context) {
	if !h.isAdmin(c.GetString("user_id")) {
		c.JSON(http.StatusForbidden, gin.H{"error": "ask 仅管理员可用"})
		return
	}
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

// Search 暴露检索接口（POST /api/v1/search）。
// 不调 LLM，返回纯结构化命中片段（content = 完整 chunk），供 Agent 自拼上下文回答
// （双接口演进 ADR-001）。按用户隔离：普通用户只搜自己的，admin 搜全库。
func (h *Handler) Search(c *gin.Context) {
	userID := h.scopeUserID(c.GetString("user_id"))
	var q model.Question
	if err := c.ShouldBindJSON(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误: " + err.Error()})
		return
	}
	resp, err := h.search.Search(c.Request.Context(), q, userID)
	if err != nil {
		logger.Error("search.request_failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	logger.Debug("search.request_done", "query", truncate(q.Query, 120),
		"hits", resp.TotalChunks, "latency_ms", resp.LatencyMS)
	c.JSON(http.StatusOK, resp)
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}
