package tests

import (
	"context"
	"math"
	"testing"

	"paper-rag-backend/internal/model"
	"paper-rag-backend/internal/store"
)

func TestMemoryStore_SearchBySimilarity(t *testing.T) {
	s := store.NewMemoryStore()
	ctx := context.Background()

	_ = s.AddChunks(ctx, []model.Chunk{
		{ID: "1", DocumentID: "d1", Filename: "a.pdf", Page: 1, Content: "关于向量数据库的原理", Vector: []float32{1, 0, 0}},
		{ID: "2", DocumentID: "d1", Filename: "a.pdf", Page: 2, Content: "关于深度学习的应用", Vector: []float32{0, 1, 0}},
		{ID: "3", DocumentID: "d2", Filename: "b.pdf", Page: 1, Content: "关于股票市场的波动", Vector: []float32{0, 0, 1}},
	})

	// 查询向量接近 chunk 1
	results, err := s.Search(ctx, []float32{0.9, 0.1, 0.1}, 3, 0)
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}
	if results[0].Chunk.ID != "1" {
		t.Fatalf("top result = %s, want 1", results[0].Chunk.ID)
	}
}

func TestMemoryStore_ThresholdFilter(t *testing.T) {
	s := store.NewMemoryStore()
	ctx := context.Background()
	_ = s.AddChunks(ctx, []model.Chunk{
		{ID: "1", Vector: []float32{1, 0}},
		{ID: "2", Vector: []float32{0, 1}},
	})
	results, err := s.Search(ctx, []float32{1, 0}, 10, 0.9)
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(results) != 1 || results[0].Chunk.ID != "1" {
		t.Fatalf("阈值过滤结果错误: %+v", results)
	}
}

func TestMemoryStore_DeleteDocument(t *testing.T) {
	s := store.NewMemoryStore()
	ctx := context.Background()
	_ = s.AddChunks(ctx, []model.Chunk{{ID: "c1", DocumentID: "d1", Vector: []float32{1, 0}}})
	_ = s.SaveDocument(ctx, &model.Document{ID: "d1", Filename: "a.pdf", Status: "ready"})

	if err := s.DeleteDocument(ctx, "d1"); err != nil {
		t.Fatalf("DeleteDocument 失败: %v", err)
	}
	doc, _ := s.GetDocument(ctx, "d1")
	if doc != nil {
		t.Fatalf("文档删除后仍存在")
	}
	results, _ := s.Search(ctx, []float32{1, 0}, 10, 0)
	if len(results) != 0 {
		t.Fatalf("关联向量未删除, got %d", len(results))
	}
}

func TestCosineSimilarity(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{0, 1, 0}
	if got := store.CosineSimilarity(a, b); got != 0 {
		t.Fatalf("正交向量相似度 = %v, want 0", got)
	}
	c := []float32{2, 0, 0}
	if got := store.CosineSimilarity(a, c); math.Abs(float64(got-1)) > 1e-6 {
		t.Fatalf("同向向量相似度 = %v, want 1", got)
	}
}
