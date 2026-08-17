// Package store 定义向量/文档存储接口，并提供基于内存的实现。
//
// 后续接入真实数据库（如 pgvector、Qdrant、Milvus）时，只需实现 Store 接口
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

// Store 统一存储接口：文档元数据 + 向量。
type Store interface {
	// ---- 文档元数据 ----
	SaveDocument(ctx context.Context, doc *model.Document) error

	GetDocument(ctx context.Context, id string) (*model.Document, error)
	ListDocuments(ctx context.Context) ([]model.Document, error)
	DeleteDocument(ctx context.Context, id string) error

	// ---- 向量 ----
	AddChunks(ctx context.Context, chunks []model.Chunk) error
	// Search 返回与 query 最相似的 topK 个片段（按余弦相似度降序）。
	Search(ctx context.Context, query []float32, topK int, threshold float32) ([]SearchResult, error)
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
