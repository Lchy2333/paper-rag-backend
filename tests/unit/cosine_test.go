package unit

import (
	"math"
	"testing"

	"paper-rag-backend/internal/store"
)

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
