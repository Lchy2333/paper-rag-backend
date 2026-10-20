package service

import (
	"context"
	"fmt"
	"time"

	"paper-rag-backend/internal/ai"
	"paper-rag-backend/internal/config"
	"paper-rag-backend/internal/logger"
	"paper-rag-backend/internal/model"
	"paper-rag-backend/internal/store"
)

// SearchService 提供"纯检索"能力（/search 接口的载体）。
//
// 与 AskService 的关系：共享 *ai.Client + *store.Store，但 SearchService 不调 ai.Chat，
// 把检索到的 chunk 直接返回，让 Agent 自拼上下文回答（双接口演进 ADR-001）。
type SearchService struct {
	ai    *ai.Client
	store store.Store
	cfg   config.RAGConfig
}

// NewSearchService 创建搜索服务。
func NewSearchService(c *ai.Client, s store.Store, cfg config.RAGConfig) *SearchService {
	return &SearchService{ai: c, store: s, cfg: cfg}
}

// SearchItem 是 /search 的响应元素（命中 chunk 的结构化信息）。
// JSON tag 与 architecture.md §ADR-001 契约一致：
//   - chunk_id / document_id / filename / page / score 与 model.Citation 同名同义
//   - content = 完整 chunk 内容（受 chunk_size 限制，默认 800 字符），
//     不给 Agent 200 字符预览——Agent 自拼上下文用
type SearchItem struct {
	ChunkID    string  `json:"chunk_id"`
	DocumentID string  `json:"document_id"`
	Filename   string  `json:"filename"`
	Page       int     `json:"page"`
	Score      float32 `json:"score"`
	Content    string  `json:"content"`
}

// SearchResponse 是 /search 的响应包装。
type SearchResponse struct {
	Results     []SearchItem `json:"results"`
	TotalChunks int          `json:"total_chunks"`
	LatencyMS   int64        `json:"latency_ms"`
}

// ProgressEvent 是检索过程的阶段性进度事件（SSE 逐条下发，供 Agent 转发给前端）。
// Stage: embed（向量化）/ retrieve（混合检索）/ rerank（精排重排）
// Status: start / done；done 时带 Ms（耗时），retrieve done 额外带 Hits（候选数）。
type ProgressEvent struct {
	Stage  string `json:"stage"`
	Status string `json:"status"`
	Ms     int64  `json:"ms,omitempty"`
	Hits   int    `json:"hits,omitempty"`
}

// Search 对问题做向量检索（强制混合），返回结构化命中片段。
// 与 AskService.Ask 共享 store.Search + 归属过滤，
// **唯一区别是不调 ai.Chat**——把 LLM 生成上浮到 Agent。
// userID 由 handler 从 X-User-ID 注入；"" 表示 admin（全库检索）。
// onProgress 为可选回调（variadic，旧调用方/测试不传也不受影响）：
// 传了则检索过程各阶段（embed/retrieve/rerank）逐条回调，供 SSE 实时下发。
func (s *SearchService) Search(ctx context.Context, q model.Question, userID string,
	onProgress ...func(ProgressEvent)) (*SearchResponse, error) {
	start := time.Now()
	emit := func(ev ProgressEvent) {
		if len(onProgress) > 0 && onProgress[0] != nil {
			onProgress[0](ev)
		}
	}
	logger.Debug("search.start", "query", truncate(q.Query, 120))

	// 1. 问题向量化（与 AskService.Ask 一致）
	emit(ProgressEvent{Stage: "embed", Status: "start"})
	embStart := time.Now()
	vectors, err := s.ai.Embed(ctx, []string{q.Query})
	if err != nil {
		logger.Error("search.embed_failed", "err", err)
		return nil, err
	}
	emit(ProgressEvent{Stage: "embed", Status: "done", Ms: time.Since(embStart).Milliseconds()})
	logger.Debug("search.embed_done", "duration_ms", time.Since(embStart).Milliseconds())

	// 2. 检索参数（与 AskService.Ask 一致：限 user_id / doc_ids，强制混合）
	topK := s.cfg.TopK
	if q.TopK > 0 {
		topK = q.TopK
	}
	// rerank 开启时先放宽检索量（粗排多捞候选），精排后再截断回 topK。
	// 请求级开关：q.Rerank 非 nil 时强制开/关，nil 时跟随 ai.rerank.enabled。
	useRerank := resolveRerank(s.ai, q.Rerank, "search")
	pool := topK
	if useRerank {
		if p := s.ai.RerankPool(); p > pool {
			pool = p
		}
	}
	searchStart := time.Now()
	emit(ProgressEvent{Stage: "retrieve", Status: "start"})
	results, err := s.store.Search(ctx, vectors[0], pool, s.cfg.SimilarityThreshold, store.SearchOptions{
		DocIDs:    q.DocumentIDs,
		UserID:    userID,
		UseHybrid: true,
		QueryText: q.Query,
	})
	if err != nil {
		logger.Error("search.search_failed", "err", err)
		return nil, fmt.Errorf("检索失败: %w", err)
	}
	emit(ProgressEvent{Stage: "retrieve", Status: "done", Ms: time.Since(searchStart).Milliseconds(), Hits: len(results)})
	logger.Debug("search.search_done", "hits", len(results), "top_k", pool,
		"duration_ms", time.Since(searchStart).Milliseconds())

	// 2.5 精排：候选超过 topK 时 cross-encoder 重打分排序；失败自动降级原顺序（fail-open）
	if useRerank && len(results) > topK {
		emit(ProgressEvent{Stage: "rerank", Status: "start"})
		rerankStart := time.Now()
		results = rerankResults(ctx, s.ai, "search", q.Query, results, topK)
		emit(ProgressEvent{Stage: "rerank", Status: "done", Ms: time.Since(rerankStart).Milliseconds()})
	}

	// 3. 拼成结构化响应（content = 完整 chunk，不截断——Agent 自拼上下文用）
	items := make([]SearchItem, 0, len(results))
	for _, r := range results {
		items = append(items, SearchItem{
			ChunkID:    r.Chunk.ID,
			DocumentID: r.Chunk.DocumentID,
			Filename:   r.Chunk.Filename,
			Page:       r.Chunk.Page,
			Score:      r.Score,
			Content:    r.Chunk.Content,
		})
	}

	resp := &SearchResponse{
		Results:     items,
		TotalChunks: len(items),
		LatencyMS:   time.Since(start).Milliseconds(),
	}
	logger.Info("search.done", "hits", len(items), "latency_ms", resp.LatencyMS)
	return resp, nil
}