package store

import (
	"context"
	"sort"
	"sync"

	"paper-rag-backend/internal/model"
)

// MemoryStore 是 Store 的内存实现，仅用于开发/跑通流程，
// 服务重启后数据丢失。生产环境请替换为持久化存储。
type MemoryStore struct {
	mu        sync.RWMutex
	documents map[string]*model.Document
	chunks    map[string]model.Chunk
}

// NewMemoryStore 创建一个空的内存存储。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		documents: make(map[string]*model.Document),
		chunks:    make(map[string]model.Chunk),
	}
}

func (m *MemoryStore) SaveDocument(_ context.Context, doc *model.Document) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.documents[doc.ID] = doc
	return nil
}

func (m *MemoryStore) GetDocument(_ context.Context, id string) (*model.Document, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	doc, ok := m.documents[id]
	if !ok {
		return nil, nil
	}
	copy := *doc
	return &copy, nil
}

func (m *MemoryStore) ListDocuments(_ context.Context) ([]model.Document, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	docs := make([]model.Document, 0, len(m.documents))
	for _, d := range m.documents {
		docs = append(docs, *d)
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].CreatedAt.After(docs[j].CreatedAt) })
	return docs, nil
}

func (m *MemoryStore) DeleteDocument(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.documents, id)
	for cid, c := range m.chunks {
		if c.DocumentID == id {
			delete(m.chunks, cid)
		}
	}
	return nil
}

func (m *MemoryStore) AddChunks(_ context.Context, chunks []model.Chunk) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range chunks {
		m.chunks[c.ID] = c
	}
	return nil
}

func (m *MemoryStore) Search(_ context.Context, query []float32, topK int, threshold float32) ([]SearchResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	results := make([]SearchResult, 0, len(m.chunks))
	for _, c := range m.chunks {
		score := CosineSimilarity(query, c.Vector)
		if score < threshold {
			continue
		}
		results = append(results, SearchResult{Chunk: c, Score: score})
	}

	sort.Slice(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if len(results) > topK {
		results = results[:topK]
	}
	return results, nil
}
