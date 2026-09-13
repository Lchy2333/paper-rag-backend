// Package store 定义向量/文档存储接口，并基于 PostgreSQL + pgvector 实现。
//
// 后续若接入其他向量数据库（如 Qdrant、Milvus），只需实现 Store 接口
// 并在 service 初始化处替换为对应实现，无需改动上层业务代码。
package store

import (
	"context"
	"math"

	"paper-rag-backend/internal/model"
)

// SearchResult 是检索命中的片段及相似度得分。
type SearchResult struct {
	Chunk model.Chunk
	Score float32
}

// SearchOptions 是检索的可选过滤条件。
type SearchOptions struct {
	DocIDs []string // 限定文档范围，为空不限
	UserID string   // 限定归属用户，为空不限

	// UseHybrid 开启混合检索（向量 + 字面）。
	// 向量路走两阶段检索（粗筛+精筛），字面路对查询关键词做 trigram 子串匹配，
	// 两路结果用 RRF 合并。解决公式/编号/专名这类"字面精确匹配"场景
	// （embedding 擅长主题语义、不擅长符号精确匹配，见 避坑指南 #7）。
	UseHybrid bool

	// QueryText 是字面检索用的原始查询文本（通常与向量化的 query 同源）。
	// 仅 UseHybrid=true 时有效；为空则跳过字面路（纯向量）。
	QueryText string
}

// Search 返回与 query 最相似的 topK 个片段（按余弦相似度降序）。
// opts 可限定文档范围（DocIDs）与归属用户（UserID），为空则不过滤。
// 内部实现采用两阶段检索：
//  1. 粗筛：放宽 LIMIT 并开启 pgvector iterative_scan，多捞候选（容忍噪声）；
//  2. 精筛：应用层按 opts 过滤粗筛结果；
//  3. 补漏：若精筛后不足 topK，用合规 DocIDs 走 document_id B-tree 精确取回补齐。
//
// 背景：HNSW 索引遍历是沿距离从近到远的，当查询带过滤条件时，索引只访问
// 内部一小部分节点（ef_search，默认 40），若这些候选过滤后不足 topK，结果会
// 偏少——不是没有，而是没扫到。两阶段策略把"尽量多捞"和"精确过滤"解耦。
type Store interface {
	// ---- 文档元数据 ----
	SaveDocument(ctx context.Context, doc *model.Document) error

	GetDocument(ctx context.Context, id string) (*model.Document, error)
	ListDocuments(ctx context.Context) ([]model.Document, error)
	DeleteDocument(ctx context.Context, id string) error
	// FindByContentHash 按内容哈希（文件 SHA-256）查找文档，用于上传去重；找不到返回 nil。
	FindByContentHash(ctx context.Context, hash string) (*model.Document, error)

	// ---- 向量 ----
	AddChunks(ctx context.Context, chunks []model.Chunk) error
	// Search 返回与 query 最相似的 topK 个片段（按余弦相似度降序）。
	// opts 可限定文档范围（DocIDs）与归属用户（UserID），为空则不过滤。
	Search(ctx context.Context, query []float32, topK int, threshold float32, opts SearchOptions) ([]SearchResult, error)
}

// CosineSimilarity 计算两个向量的余弦相似度。
func CosineSimilarity(a, b []float32) float32 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}
