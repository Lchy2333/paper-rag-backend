// search_progress_test.go 覆盖 /search 的 SSE 进度事件（方案②核心）：
// 检索过程必须按真实顺序吐出 embed → retrieve → rerank 的 start/done 事件，
// retrieve done 携带候选数，rerank 只在开启且候选超池时出现。
package unit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"paper-rag-backend/internal/config"
	"paper-rag-backend/internal/model"
	"paper-rag-backend/internal/service"
)

func TestSearchService_EmitsProgressEventsInOrder(t *testing.T) {
	embedSrv := newEmbedMock(t)
	defer embedSrv.Close()

	rerankSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer rerankSrv.Close()

	svc := service.NewSearchService(
		newRerankTestClient(t, rerankSrv.URL),
		&rerankMockStore{results: rerankTestResults(30)},
		config.RAGConfig{TopK: 5, SimilarityThreshold: 0},
	)

	var events []service.ProgressEvent
	_, err := svc.Search(context.Background(), model.Question{Query: "测试问题"}, "",
		func(ev service.ProgressEvent) { events = append(events, ev) })
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}

	want := []service.ProgressEvent{
		{Stage: "embed", Status: "start"},
		{Stage: "embed", Status: "done"},
		{Stage: "retrieve", Status: "start"},
		{Stage: "retrieve", Status: "done", Hits: 30},
		{Stage: "rerank", Status: "start"},
		{Stage: "rerank", Status: "done"},
	}
	if len(events) != len(want) {
		t.Fatalf("progress 事件数 = %d, want %d: %+v", len(events), len(want), events)
	}
	for i, ev := range events {
		w := want[i]
		if ev.Stage != w.Stage || ev.Status != w.Status {
			t.Fatalf("事件[%d] = %s/%s, want %s/%s", i, ev.Stage, ev.Status, w.Stage, w.Status)
		}
	}
	if events[3].Hits != 30 {
		t.Fatalf("retrieve done hits = %d, want 30", events[3].Hits)
	}
}

func TestSearchService_NoRerankProgressWhenDisabled(t *testing.T) {
	embedSrv := newEmbedMock(t)
	defer embedSrv.Close()

	svc := service.NewSearchService(
		newRerankTestClient(t, "http://unused-invalid"),
		&rerankMockStore{results: rerankTestResults(3)}, // 3 条 ≤ top_k=5，本就无需精排
		config.RAGConfig{TopK: 5, SimilarityThreshold: 0},
	)

	var events []service.ProgressEvent
	_, err := svc.Search(context.Background(), model.Question{Query: "测试问题"}, "",
		func(ev service.ProgressEvent) { events = append(events, ev) })
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	for _, ev := range events {
		if ev.Stage == "rerank" {
			t.Fatalf("rerank 关闭时应无 rerank 进度事件: %+v", events)
		}
	}
}
