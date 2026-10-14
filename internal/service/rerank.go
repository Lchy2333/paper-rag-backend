// rerank.go 提供 /ask 与 /search 共用的精排助手：
// 粗排候选交给 cross-encoder 重打分，按返回下标重排并截断；失败降级为原顺序（fail-open）。
//
// 与 AskService / SearchService 的关系：纯函数、无状态，不引入两服务间的耦合
// （保持 ADR-001「Ask/Search 平行演进」的约定）。
package service

import (
	"context"

	"paper-rag-backend/internal/ai"
	"paper-rag-backend/internal/logger"
	"paper-rag-backend/internal/store"
)

// rerankResults 用 rerank 模型对粗排候选精排，返回按相关性降序、至多 topN 条的结果。
//
// 行为约定：
//   - topK 是本次请求的目标条数（请求级 top_k 或 rag.top_k）；
//     ai.rerank.top_n > 0 时作为截断上限（不超过 topK）
//   - rerank 调用失败 / 返回空结果：降级为原顺序截断（fail-open，绝不阻塞问答），
//     并打 warn 暴露降级（与 store 里 tsv/lex 降级同哲学）
//   - 成功：按返回下标回查候选数组重排，Score 替换为 rerank 相关性分
//     （量纲从余弦/RRF 分切换，前端/Agent 拿到的是 0~1 相关性分）
func rerankResults(ctx context.Context, client *ai.Client, stage, query string,
	results []store.SearchResult, topK int) []store.SearchResult {

	if topK <= 0 || len(results) == 0 {
		return results
	}
	topN := topK
	if n := client.RerankTopN(); n > 0 && n < topN {
		topN = n
	}

	contents := make([]string, len(results))
	for i, r := range results {
		contents[i] = r.Chunk.Content
	}

	reranked, err := client.Rerank(ctx, query, contents, topN)
	if err != nil {
		logger.Warn("rerank.failed", "stage", stage, "err", err)
		return truncateResults(results, topN)
	}

	out := make([]store.SearchResult, 0, len(reranked))
	for _, rr := range reranked {
		if rr.Index < 0 || rr.Index >= len(results) {
			continue // ai 层已防御越界下标，这里再挡一道
		}
		r := results[rr.Index]
		r.Score = rr.Score
		out = append(out, r)
	}
	// 本地强制截断：不依赖 API 遵守 top_n（防御返回超量）
	if len(out) > topN {
		out = out[:topN]
	}
	if len(out) == 0 {
		// API 未返回任何有效结果：视同降级，不返回空集
		logger.Warn("rerank.empty", "stage", stage)
		return truncateResults(results, topN)
	}
	return out
}

// resolveRerank 解析请求级 rerank 开关（/ask、/search 共用）：
//   - override == nil  ：跟随配置 ai.rerank.enabled（老调用方不传字段，零回归）
//   - override == true ：强制开（要求 ai.rerank.model 已配置，否则无从调用，回退关闭并打 warn）
//   - override == false：强制关（临时关闭做对比/调试，不动配置）
func resolveRerank(client *ai.Client, override *bool, stage string) bool {
	if override == nil {
		return client.RerankEnabled()
	}
	if *override {
		if client.RerankConfigured() {
			return true
		}
		logger.Warn("rerank.force_on_ignored", "stage", stage,
			"reason", "ai.rerank.model 未配置，强制开启被忽略")
	}
	return false
}

// truncateResults 按原顺序截断（降级路径与 top_n 覆盖共用）。
func truncateResults(results []store.SearchResult, topK int) []store.SearchResult {
	if len(results) > topK {
		return results[:topK]
	}
	return results
}
