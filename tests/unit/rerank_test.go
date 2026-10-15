// rerank_test.go 覆盖 rerank 精排的两层逻辑（全部走本地 httptest 假服务，无外部依赖）：
//   1. 客户端层（ai.Client.Rerank）：请求构造（路径/鉴权/请求体）、按分数重排、越界下标跳过、
//      HTTP 错误传递、未配置报错、getter 默认值
//   2. 服务层（SearchService.Search 端到端）：rerank 开启时扩池检索+精排截断+分数替换、
//      rerank 失败降级原顺序（fail-open）、未开启时零回归（不扩池、不调 rerank）、top_n 截断上限
package unit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"paper-rag-backend/internal/ai"
	"paper-rag-backend/internal/config"
	"paper-rag-backend/internal/model"
	"paper-rag-backend/internal/service"
	"paper-rag-backend/internal/store"
)

// ---- 客户端层 ----

func TestRerankClient_RequestAndReorder(t *testing.T) {
	var mu sync.Mutex
	var gotAuth, gotPath string
	var gotBody struct {
		Model           string   `json:"model"`
		Query           string   `json:"query"`
		Documents       []string `json:"documents"`
		TopN            int      `json:"top_n"`
		ReturnDocuments bool     `json:"return_documents"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		// 故意乱序返回，验证客户端按分数降序重排
		_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":0.4},{"index":2,"relevance_score":0.9}]}`))
	}))
	defer srv.Close()

	client := ai.NewClient(config.AIConfig{
		BaseURL: "http://unused-invalid", APIKey: "fallback-key",
		Embedding: config.EmbeddingConfig{Model: "m"},
		Chat:      config.ChatConfig{Model: "c"},
		Rerank: config.RerankConfig{
			Enabled: true, Model: "rerank-model",
			BaseURL: srv.URL, APIKey: "rerank-key",
			TimeoutSec: 5,
		},
	})

	out, err := client.Rerank(context.Background(), "测试问题", []string{"a", "b", "c"}, 2)
	if err != nil {
		t.Fatalf("Rerank 失败: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/rerank" {
		t.Fatalf("请求路径 = %q, want /rerank", gotPath)
	}
	if gotAuth != "Bearer rerank-key" {
		t.Fatalf("Authorization = %q, want Bearer rerank-key", gotAuth)
	}
	if gotBody.Model != "rerank-model" || gotBody.Query != "测试问题" ||
		len(gotBody.Documents) != 3 || gotBody.TopN != 2 {
		t.Fatalf("请求体不对: %+v", gotBody)
	}
	if gotBody.ReturnDocuments {
		t.Fatal("return_documents 应为 false")
	}
	if len(out) != 2 || out[0].Index != 2 || out[0].Score != 0.9 || out[1].Index != 0 {
		t.Fatalf("重排结果不对（应按分数降序）: %+v", out)
	}
}

func TestRerankClient_SkipsOutOfBoundsIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 999 越界（只传了 3 个文档），应被跳过而不是 panic
		_, _ = w.Write([]byte(`{"results":[{"index":999,"relevance_score":0.99},{"index":1,"relevance_score":0.5},{"index":-1,"relevance_score":0.9}]}`))
	}))
	defer srv.Close()

	client := newRerankTestClient(t, srv.URL)
	out, err := client.Rerank(context.Background(), "q", []string{"a", "b", "c"}, 3)
	if err != nil {
		t.Fatalf("Rerank 失败: %v", err)
	}
	if len(out) != 1 || out[0].Index != 1 {
		t.Fatalf("越界下标应被跳过, got %+v", out)
	}
}

func TestRerankClient_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	client := newRerankTestClient(t, srv.URL)
	_, err := client.Rerank(context.Background(), "q", []string{"a"}, 1)
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("应返回含 HTTP 429 的错误, got %v", err)
	}
}

func TestRerankClient_NotConfigured(t *testing.T) {
	client := ai.NewClient(config.AIConfig{
		Embedding: config.EmbeddingConfig{Model: "m"},
		Chat:      config.ChatConfig{Model: "c"},
	})
	if client.RerankEnabled() {
		t.Fatal("未配置时 RerankEnabled 应为 false")
	}
	if _, err := client.Rerank(context.Background(), "q", []string{"a"}, 1); err == nil {
		t.Fatal("未配置时 Rerank 应报错")
	}
	// 空候选直接返回，不发请求
	if out, err := client.Rerank(context.Background(), "q", nil, 1); out != nil || err != nil {
		t.Fatalf("空候选应返回 nil/nil, got %v/%v", out, err)
	}
}

func TestRerankClient_Getters(t *testing.T) {
	c1 := ai.NewClient(config.AIConfig{Rerank: config.RerankConfig{}})
	if c1.RerankPool() != 30 {
		t.Fatalf("默认池 = %d, want 30", c1.RerankPool())
	}
	if c1.RerankTopN() != 0 {
		t.Fatalf("默认 top_n = %d, want 0（沿用 rag.top_k）", c1.RerankTopN())
	}
	c2 := ai.NewClient(config.AIConfig{Rerank: config.RerankConfig{CandidatePool: 50, TopN: 3}})
	if c2.RerankPool() != 50 || c2.RerankTopN() != 3 {
		t.Fatalf("getter 未读配置: pool=%d top_n=%d, want 50/3", c2.RerankPool(), c2.RerankTopN())
	}
}

// ---- 服务层（SearchService 端到端：mock embedding + mock store + mock rerank） ----

// rerankMockStore 记录 Search 调用参数的假存储。
type rerankMockStore struct {
	mu         sync.Mutex
	searchTopK int
	results    []store.SearchResult
}

func (m *rerankMockStore) SaveDocument(ctx context.Context, doc *model.Document) error { return nil }
func (m *rerankMockStore) GetDocument(ctx context.Context, id string) (*model.Document, error) {
	return nil, nil
}
func (m *rerankMockStore) ListDocuments(ctx context.Context) ([]model.Document, error) { return nil, nil }
func (m *rerankMockStore) DeleteDocument(ctx context.Context, id string) error         { return nil }
func (m *rerankMockStore) FindByContentHash(ctx context.Context, hash string) (*model.Document, error) {
	return nil, nil
}
func (m *rerankMockStore) AddChunks(ctx context.Context, chunks []model.Chunk) error { return nil }
func (m *rerankMockStore) DeleteChunks(ctx context.Context, documentID string) error { return nil }
func (m *rerankMockStore) Search(ctx context.Context, query []float32, topK int, threshold float32, opts store.SearchOptions) ([]store.SearchResult, error) {
	m.mu.Lock()
	m.searchTopK = topK
	m.mu.Unlock()
	out := m.results
	if len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

var _ store.Store = (*rerankMockStore)(nil)

// 异步摄入队列的桩实现（rerank 测试不关心队列行为，返回"队列空"即可）。
func (m *rerankMockStore) ClaimPendingTask(ctx context.Context) (*model.Document, error) {
	return nil, nil
}

func (m *rerankMockStore) ResetProcessingTasks(ctx context.Context) (int, error) {
	return 0, nil
}

// newEmbedMock 起一个返回固定向量的 OpenAI 兼容 /embeddings 假服务。
func newEmbedMock(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"object": "embedding", "embedding": []float64{0.1, 0.2}, "index": 0}},
			"model":  "test-embed",
		})
	}))
}

// rerankTestResults 造 n 条候选：ID 按序编号，原始分数 = 下标（便于断言是否被 rerank 改写）。
func rerankTestResults(n int) []store.SearchResult {
	out := make([]store.SearchResult, n)
	for i := range out {
		out[i] = store.SearchResult{
			Chunk: model.Chunk{
				ID:      fmt.Sprintf("chunk-%02d", i),
				Content: fmt.Sprintf("第 %d 号候选内容", i),
			},
			Score: float32(i),
		}
	}
	return out
}

func TestSearchService_RerankEnabled(t *testing.T) {
	embedSrv := newEmbedMock(t)
	defer embedSrv.Close()

	var rerankCalls int32
	rerankSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rerankCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"index":7,"relevance_score":0.9},{"index":0,"relevance_score":0.4},{"index":999,"relevance_score":0.99}]}`))
	}))
	defer rerankSrv.Close()

	svc := service.NewSearchService(
		newRerankTestClient(t, rerankSrv.URL),
		&rerankMockStore{results: rerankTestResults(30)},
		config.RAGConfig{TopK: 5, SimilarityThreshold: 0},
	)

	resp, err := svc.Search(context.Background(), model.Question{Query: "测试问题"})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}

	// 粗排扩池断言见 TestSearchService_RerankPoolExpanded
	if got := atomic.LoadInt32(&rerankCalls); got != 1 {
		t.Fatalf("rerank 调用次数 = %d, want 1", got)
	}
	// 重排 + 分数替换 + 越界下标（999）跳过
	if len(resp.Results) != 2 {
		t.Fatalf("results = %d 条, want 2", len(resp.Results))
	}
	if resp.Results[0].ChunkID != "chunk-07" || resp.Results[0].Score != 0.9 {
		t.Fatalf("results[0] = %s/%v, want chunk-07/0.9", resp.Results[0].ChunkID, resp.Results[0].Score)
	}
	if resp.Results[1].ChunkID != "chunk-00" || resp.Results[1].Score != 0.4 {
		t.Fatalf("results[1] = %s/%v, want chunk-00/0.4", resp.Results[1].ChunkID, resp.Results[1].Score)
	}
	if resp.TotalChunks != 2 {
		t.Fatalf("total_chunks = %d, want 2（精排后条数）", resp.TotalChunks)
	}
}

func TestSearchService_RerankPoolExpanded(t *testing.T) {
	embedSrv := newEmbedMock(t)
	defer embedSrv.Close()

	rerankSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer rerankSrv.Close()

	ms := &rerankMockStore{results: rerankTestResults(30)}
	svc := service.NewSearchService(
		newRerankTestClient(t, rerankSrv.URL),
		ms,
		config.RAGConfig{TopK: 5, SimilarityThreshold: 0},
	)
	if _, err := svc.Search(context.Background(), model.Question{Query: "q"}); err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.searchTopK != 30 {
		t.Fatalf("store.Search topK = %d, want 30（rerank 开启时应扩池）", ms.searchTopK)
	}
}

func TestSearchService_RerankDegradesOnError(t *testing.T) {
	embedSrv := newEmbedMock(t)
	defer embedSrv.Close()

	rerankSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer rerankSrv.Close()

	svc := service.NewSearchService(
		newRerankTestClient(t, rerankSrv.URL),
		&rerankMockStore{results: rerankTestResults(30)},
		config.RAGConfig{TopK: 5, SimilarityThreshold: 0},
	)

	resp, err := svc.Search(context.Background(), model.Question{Query: "测试问题"})
	if err != nil {
		t.Fatalf("rerank 失败应降级而非报错: %v", err)
	}
	// 降级 = 原顺序前 5 条，分数保持粗排原始分（未被 rerank 改写）
	if len(resp.Results) != 5 {
		t.Fatalf("results = %d, want 5", len(resp.Results))
	}
	for i, item := range resp.Results {
		wantID := fmt.Sprintf("chunk-%02d", i)
		if item.ChunkID != wantID {
			t.Fatalf("results[%d] = %s, want %s（原顺序）", i, item.ChunkID, wantID)
		}
		if item.Score != float32(i) {
			t.Fatalf("results[%d].score = %v, want 原始分 %v", i, item.Score, float32(i))
		}
	}
}

func TestSearchService_RerankDisabledZeroRegression(t *testing.T) {
	embedSrv := newEmbedMock(t)
	defer embedSrv.Close()

	var rerankCalls int32
	rerankSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rerankCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer rerankSrv.Close()

	// enabled=false：rerank 服务配置了也不该被调用
	client := ai.NewClient(config.AIConfig{
		BaseURL: embedSrv.URL, APIKey: "k",
		Embedding: config.EmbeddingConfig{Model: "m"},
		Chat:      config.ChatConfig{Model: "c"},
		Rerank: config.RerankConfig{
			Enabled: false, Model: "rerank-model",
			BaseURL: rerankSrv.URL, CandidatePool: 30,
		},
	})
	ms := &rerankMockStore{results: rerankTestResults(30)}
	svc := service.NewSearchService(client, ms, config.RAGConfig{TopK: 5, SimilarityThreshold: 0})

	resp, err := svc.Search(context.Background(), model.Question{Query: "测试问题"})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	// 不扩池：store.Search 收到 topK=5
	ms.mu.Lock()
	gotTopK := ms.searchTopK
	ms.mu.Unlock()
	if gotTopK != 5 {
		t.Fatalf("store.Search topK = %d, want 5（未开启 rerank 不扩池）", gotTopK)
	}
	// rerank 服务一次都没被调用
	if got := atomic.LoadInt32(&rerankCalls); got != 0 {
		t.Fatalf("rerank 调用次数 = %d, want 0（零回归）", got)
	}
	// 结果 = 粗排原顺序前 5 条
	if len(resp.Results) != 5 || resp.Results[0].ChunkID != "chunk-00" {
		t.Fatalf("结果应为原顺序前 5 条, got %d 条, first=%s", len(resp.Results), resp.Results[0].ChunkID)
	}
}

func TestSearchService_RerankTopNCap(t *testing.T) {
	embedSrv := newEmbedMock(t)
	defer embedSrv.Close()

	// rerank 返回 3 条有效结果，但 ai.rerank.top_n=2 → 最多保留 2 条
	rerankSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"index":3,"relevance_score":0.9},{"index":1,"relevance_score":0.7},{"index":2,"relevance_score":0.5}]}`))
	}))
	defer rerankSrv.Close()

	client := ai.NewClient(config.AIConfig{
		BaseURL: embedSrv.URL, APIKey: "k",
		Embedding: config.EmbeddingConfig{Model: "m"},
		Chat:      config.ChatConfig{Model: "c"},
		Rerank: config.RerankConfig{
			Enabled: true, Model: "rerank-model",
			BaseURL: rerankSrv.URL, CandidatePool: 30, TopN: 2, TimeoutSec: 5,
		},
	})
	svc := service.NewSearchService(client,
		&rerankMockStore{results: rerankTestResults(30)},
		config.RAGConfig{TopK: 5, SimilarityThreshold: 0},
	)

	resp, err := svc.Search(context.Background(), model.Question{Query: "测试问题"})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %d, want 2（top_n 截断上限）", len(resp.Results))
	}
	if resp.Results[0].ChunkID != "chunk-03" || resp.Results[1].ChunkID != "chunk-01" {
		t.Fatalf("top_n 截断后应保留分数最高的 2 条: %s, %s",
			resp.Results[0].ChunkID, resp.Results[1].ChunkID)
	}
}

// newRerankTestClient 构造指向 rerankSrv 的客户端（嵌入服务用不上的占位配置）。
func newRerankTestClient(t *testing.T, rerankURL string) *ai.Client {
	t.Helper()
	embedSrv := newEmbedMock(t)
	t.Cleanup(embedSrv.Close)
	return ai.NewClient(config.AIConfig{
		BaseURL: embedSrv.URL, APIKey: "k",
		Embedding: config.EmbeddingConfig{Model: "m"},
		Chat:      config.ChatConfig{Model: "c"},
		Rerank: config.RerankConfig{
			Enabled: true, Model: "rerank-model",
			BaseURL: rerankURL, APIKey: "rk", CandidatePool: 30, TimeoutSec: 5,
		},
	})
}

// TestRerankClient_LiveSmoke 真实 SiliconFlow API 冒烟测试：
// 设置 SILICONFLOW_API_KEY 后才运行（默认跳过，不依赖外网）。
// 验证鉴权头、模型名、响应解析的端到端正确性。
func TestRerankClient_LiveSmoke(t *testing.T) {
	apiKey := os.Getenv("SILICONFLOW_API_KEY")
	if apiKey == "" {
		t.Skip("未设置 SILICONFLOW_API_KEY，跳过真实 API 冒烟测试")
	}
	client := ai.NewClient(config.AIConfig{
		Rerank: config.RerankConfig{
			Enabled:    true,
			Model:      "BAAI/bge-reranker-v2-m3",
			BaseURL:    "https://api.siliconflow.cn/v1",
			APIKey:     apiKey,
			TimeoutSec: 15,
		},
	})
	out, err := client.Rerank(context.Background(),
		"钢管混凝土柱的受剪承载力计算公式是什么",
		[]string{
			"斜截面受剪承载力按下式计算：V=0.5πtf'Dcotθ，其中 f 为混凝土抗拉强度，D 为钢管外径。",
			"深度学习模型在图像识别任务中取得显著进展。",
		}, 2)
	if err != nil {
		t.Fatalf("真实 rerank 调用失败: %v", err)
	}
	if len(out) == 0 || out[0].Index != 0 || out[0].Score < 0.5 {
		t.Fatalf("冒烟结果异常（正解应排第一且分数较高）: %+v", out)
	}
	t.Logf("冒烟通过: top1 index=%d score=%.4f", out[0].Index, out[0].Score)
}

// ---- 请求级 rerank 开关（question.rerank 三态） ----

func boolPtr(b bool) *bool { return &b }

// 请求 rerank=true：配置 enabled=false 但 model 已配置 → 强制开启。
func TestSearchService_RerankRequestForceOn(t *testing.T) {
	embedSrv := newEmbedMock(t)
	defer embedSrv.Close()

	var rerankCalls int32
	rerankSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rerankCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"index":7,"relevance_score":0.9}]}`))
	}))
	defer rerankSrv.Close()

	client := ai.NewClient(config.AIConfig{
		BaseURL: embedSrv.URL, APIKey: "k",
		Embedding: config.EmbeddingConfig{Model: "m"},
		Chat:      config.ChatConfig{Model: "c"},
		Rerank: config.RerankConfig{
			Enabled: false, Model: "rerank-model", // 配置关，但有模型
			BaseURL: rerankSrv.URL, CandidatePool: 30, TimeoutSec: 5,
		},
	})
	ms := &rerankMockStore{results: rerankTestResults(30)}
	svc := service.NewSearchService(client, ms, config.RAGConfig{TopK: 5, SimilarityThreshold: 0})

	resp, err := svc.Search(context.Background(), model.Question{Query: "测试问题", Rerank: boolPtr(true)})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if got := atomic.LoadInt32(&rerankCalls); got != 1 {
		t.Fatalf("rerank 调用次数 = %d, want 1（请求强制开启）", got)
	}
	ms.mu.Lock()
	gotTopK := ms.searchTopK
	ms.mu.Unlock()
	if gotTopK != 30 {
		t.Fatalf("store.Search topK = %d, want 30（强制开启应扩池）", gotTopK)
	}
	if len(resp.Results) != 1 || resp.Results[0].ChunkID != "chunk-07" || resp.Results[0].Score != 0.9 {
		t.Fatalf("应为 rerank 后的结果: %+v", resp.Results)
	}
}

// 请求 rerank=false：配置 enabled=true → 强制关闭（零回归验证：不扩池、不调 rerank）。
func TestSearchService_RerankRequestForceOff(t *testing.T) {
	embedSrv := newEmbedMock(t)
	defer embedSrv.Close()

	var rerankCalls int32
	rerankSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rerankCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"index":7,"relevance_score":0.9}]}`))
	}))
	defer rerankSrv.Close()

	client := ai.NewClient(config.AIConfig{
		BaseURL: embedSrv.URL, APIKey: "k",
		Embedding: config.EmbeddingConfig{Model: "m"},
		Chat:      config.ChatConfig{Model: "c"},
		Rerank: config.RerankConfig{
			Enabled: true, Model: "rerank-model", // 配置开
			BaseURL: rerankSrv.URL, CandidatePool: 30, TimeoutSec: 5,
		},
	})
	ms := &rerankMockStore{results: rerankTestResults(30)}
	svc := service.NewSearchService(client, ms, config.RAGConfig{TopK: 5, SimilarityThreshold: 0})

	resp, err := svc.Search(context.Background(), model.Question{Query: "测试问题", Rerank: boolPtr(false)})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if got := atomic.LoadInt32(&rerankCalls); got != 0 {
		t.Fatalf("rerank 调用次数 = %d, want 0（请求强制关闭）", got)
	}
	ms.mu.Lock()
	gotTopK := ms.searchTopK
	ms.mu.Unlock()
	if gotTopK != 5 {
		t.Fatalf("store.Search topK = %d, want 5（强制关闭不扩池）", gotTopK)
	}
	if len(resp.Results) != 5 || resp.Results[0].ChunkID != "chunk-00" {
		t.Fatalf("应为粗排原顺序前 5 条: %d 条, first=%s", len(resp.Results), resp.Results[0].ChunkID)
	}
}

// 请求 rerank=true 但 model 未配置 → 强开被忽略（回退关闭），不报错。
func TestSearchService_RerankRequestForceOnWithoutModel(t *testing.T) {
	embedSrv := newEmbedMock(t)
	defer embedSrv.Close()

	var rerankCalls int32
	rerankSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rerankCalls, 1)
	}))
	defer rerankSrv.Close()

	client := ai.NewClient(config.AIConfig{
		BaseURL: embedSrv.URL, APIKey: "k",
		Embedding: config.EmbeddingConfig{Model: "m"},
		Chat:      config.ChatConfig{Model: "c"},
		Rerank: config.RerankConfig{
			Enabled: false, Model: "", // 没配模型，无从调用
			BaseURL: rerankSrv.URL, CandidatePool: 30,
		},
	})
	if client.RerankConfigured() {
		t.Fatal("model 为空时 RerankConfigured 应为 false")
	}
	ms := &rerankMockStore{results: rerankTestResults(30)}
	svc := service.NewSearchService(client, ms, config.RAGConfig{TopK: 5, SimilarityThreshold: 0})

	resp, err := svc.Search(context.Background(), model.Question{Query: "测试问题", Rerank: boolPtr(true)})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if got := atomic.LoadInt32(&rerankCalls); got != 0 {
		t.Fatalf("rerank 调用次数 = %d, want 0（未配置模型，强开被忽略）", got)
	}
	if len(resp.Results) != 5 {
		t.Fatalf("results = %d, want 5（回退为粗排结果）", len(resp.Results))
	}
}

// 请求不传 rerank（nil）→ 跟随配置（配置开则开、配置关则关）。
func TestSearchService_RerankRequestNilFollowsConfig(t *testing.T) {
	embedSrv := newEmbedMock(t)
	defer embedSrv.Close()

	var rerankCalls int32
	rerankSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rerankCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"index":7,"relevance_score":0.9}]}`))
	}))
	defer rerankSrv.Close()

	// 配置开 + 不传 → 走 rerank
	client := ai.NewClient(config.AIConfig{
		BaseURL: embedSrv.URL, APIKey: "k",
		Embedding: config.EmbeddingConfig{Model: "m"},
		Chat:      config.ChatConfig{Model: "c"},
		Rerank:    config.RerankConfig{Enabled: true, Model: "rerank-model", BaseURL: rerankSrv.URL, CandidatePool: 30, TimeoutSec: 5},
	})
	svc := service.NewSearchService(client,
		&rerankMockStore{results: rerankTestResults(30)},
		config.RAGConfig{TopK: 5, SimilarityThreshold: 0})
	if _, err := svc.Search(context.Background(), model.Question{Query: "q"}); err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if got := atomic.LoadInt32(&rerankCalls); got != 1 {
		t.Fatalf("nil 时应跟随配置（开启）, calls = %d, want 1", got)
	}
}
