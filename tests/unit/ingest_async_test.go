// ingest_async_test.go 覆盖异步摄入的两层逻辑（全本地 mock，无外部依赖）：
//  1. worker 池（internal/worker）：门铃唤醒处理队列、兜底轮询、Drain 超时重置、Recover
//  2. 摄入服务（service.IngestService）：Accept 建.pending 记录/去重/拒非 PDF，
//     Run 的幂等清场（先 DeleteChunks）与失败标记
package unit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"paper-rag-backend/internal/config"
	"paper-rag-backend/internal/model"
	"paper-rag-backend/internal/render"
	"paper-rag-backend/internal/service"
	"paper-rag-backend/internal/worker"
)

// ---- mock：可编程 runner + 队列 store ----

// fakeRunner 记录 Run 调用；started 每次 Run 进入时发信号，block 非 nil 时阻塞到关闭（测 Drain 超时）。
type fakeRunner struct {
	mu      sync.Mutex
	calls   []string
	started chan struct{}
	block   chan struct{}
	err     error
}

func (f *fakeRunner) Run(ctx context.Context, docID string) error {
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	f.calls = append(f.calls, docID)
	f.mu.Unlock()
	return f.err
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// queueMockStore 内嵌 rerankMockStore 补齐 Store 接口；可编程抢单队列与重置记录。
type queueMockStore struct {
	*rerankMockStore
	mu       sync.Mutex
	queue    []*model.Document
	resets   int
	resetArg int // 每次 ResetProcessingTasks 返回的条数（模拟"发现"的 processing 悬案数）
}

func (m *queueMockStore) ClaimPendingTask(ctx context.Context) (*model.Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.queue) == 0 {
		return nil, nil
	}
	d := *m.queue[0] // 拷贝：模拟真实抢单改状态不影响队列里其他元素
	m.queue = m.queue[1:]
	d.Status = "processing"
	return &d, nil
}

func (m *queueMockStore) ResetProcessingTasks(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resets++
	return m.resetArg, nil
}

func newQueueStore(docs ...*model.Document) *queueMockStore {
	return &queueMockStore{rerankMockStore: &rerankMockStore{}, queue: docs}
}

func pendDoc(id string) *model.Document {
	return &model.Document{ID: id, Filename: id + ".pdf", Status: "pending", ChunkSize: 800, ChunkOverlap: 150}
}

// waitFor 轮询等待条件成立（超时失败），替代裸 Sleep。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", msg)
}

// ---- worker 池 ----

// 门铃唤醒：入队 2 个文档，Kick 一次，1 个 worker 串行处理完全部（趁热打劫直到队列空）。
func TestWorkerPool_KickProcessesWholeQueue(t *testing.T) {
	runner := &fakeRunner{}
	ms := newQueueStore(pendDoc("doc-1"), pendDoc("doc-2"))
	pool := worker.NewPool(runner, ms, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	pool.Kick()

	waitFor(t, 2*time.Second, func() bool { return runner.callCount() == 2 }, "2 个任务应被处理")
	// 队列应被排空（无重复领取）
	ms.mu.Lock()
	remain := len(ms.queue)
	ms.mu.Unlock()
	if remain != 0 {
		t.Fatalf("队列剩余 = %d, want 0", remain)
	}
	cancel()
	pool.Drain(time.Second)
}

// 兜底轮询：不按门铃，靠短轮询间隔也能发现并处理任务（丢铃最多晚一个周期）。
func TestWorkerPool_PollFallback(t *testing.T) {
	runner := &fakeRunner{}
	ms := newQueueStore(pendDoc("doc-1"))
	pool := worker.NewPool(runner, ms, 1).SetPollInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	// 注意：不 Kick

	waitFor(t, 2*time.Second, func() bool { return runner.callCount() == 1 }, "兜底轮询应处理任务")
	cancel()
	pool.Drain(time.Second)
}

// Drain 超时：在途任务阻塞超过宽限期 → 重置 processing 回 pending（由下次启动续跑）。
func TestWorkerPool_DrainTimeoutResets(t *testing.T) {
	runner := &fakeRunner{
		started: make(chan struct{}, 1),
		block:   make(chan struct{}), // Run 永远阻塞，直到测试放行
	}
	ms := newQueueStore(pendDoc("doc-1"))
	pool := worker.NewPool(runner, ms, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	pool.Kick()

	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("任务未被领取")
	}

	cancel()                    // 停止抢单
	pool.Drain(50 * time.Millisecond) // 宽限远小于任务阻塞时间 → 走超时重置

	ms.mu.Lock()
	resets := ms.resets
	ms.mu.Unlock()
	if resets != 1 {
		t.Fatalf("Drain 超时应重置一次, got %d", resets)
	}
	close(runner.block) // 放行在途任务，避免测试退出后 goroutine 泄漏
}

// Recover：启动恢复调用一次重置并返回悬案数。
func TestWorkerPool_Recover(t *testing.T) {
	ms := newQueueStore()
	ms.resetArg = 3
	pool := worker.NewPool(&fakeRunner{}, ms, 1)

	n, err := pool.Recover()
	if err != nil {
		t.Fatalf("Recover 失败: %v", err)
	}
	if n != 3 {
		t.Fatalf("Recover 返回 = %d, want 3", n)
	}
	if ms.resets != 1 {
		t.Fatalf("ResetProcessingTasks 调用 = %d, want 1", ms.resets)
	}
}

// ---- 摄入服务：Accept / Run ----

// recordStore 记录 SaveDocument / DeleteChunks / FindByContentHash 调用（Accept/Run 测试用）。
type recordStore struct {
	*rerankMockStore
	mu        sync.Mutex
	saved     []*model.Document
	deleted   []string
	byHash    *model.Document
	byID      *model.Document
	addChunks [][]model.Chunk
}

func (r *recordStore) SaveDocument(ctx context.Context, doc *model.Document) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saved = append(r.saved, doc)
	return nil
}

func (r *recordStore) DeleteChunks(ctx context.Context, documentID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, documentID)
	return nil
}

func (r *recordStore) AddChunks(ctx context.Context, chunks []model.Chunk) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addChunks = append(r.addChunks, chunks)
	return nil
}

func (r *recordStore) FindByContentHash(ctx context.Context, hash string) (*model.Document, error) {
	return r.byHash, nil
}

func (r *recordStore) GetDocument(ctx context.Context, id string) (*model.Document, error) {
	return r.byID, nil
}

func newIngestSvc(t *testing.T, rs *recordStore) *service.IngestService {
	t.Helper()
	return service.NewIngestService(
		nil, // ai.Client：Accept/失败路径不调用，nil 即可
		rs,
		config.ServerConfig{UploadDir: t.TempDir()},
		config.RAGConfig{ChunkSize: 800, ChunkOverlap: 150},
		render.Config{}, // PythonCmd 为空 → 跳过公式截图
	)
}

// Accept 成功：建 pending 记录（= 入队）+ 文件落盘且内容一致。
func TestIngestService_AcceptQueuesPending(t *testing.T) {
	dir := t.TempDir()
	rs := &recordStore{rerankMockStore: &rerankMockStore{}}
	svc := service.NewIngestService(nil, rs,
		config.ServerConfig{UploadDir: dir},
		config.RAGConfig{ChunkSize: 800, ChunkOverlap: 150},
		render.Config{})

	content := []byte("%PDF-1.4 fake content for test")
	doc, err := svc.Accept(context.Background(), bytes.NewReader(content), int64(len(content)), "test.pdf", nil)
	if err != nil {
		t.Fatalf("Accept 失败: %v", err)
	}
	if doc.Status != "pending" {
		t.Fatalf("status = %q, want pending", doc.Status)
	}
	if len(rs.saved) != 1 || rs.saved[0].Status != "pending" {
		t.Fatalf("应保存一条 pending 记录: %+v", rs.saved)
	}
	data, err := os.ReadFile(filepath.Join(dir, doc.ID+".pdf"))
	if err != nil {
		t.Fatalf("文件应落盘: %v", err)
	}
	if !bytes.Equal(data, content) {
		t.Fatalf("落盘内容不一致: %d bytes", len(data))
	}
}

// Accept 去重：内容哈希命中已有文档 → 当场拒绝，文件清理，不入队。
func TestIngestService_AcceptDuplicate(t *testing.T) {
	rs := &recordStore{
		rerankMockStore: &rerankMockStore{},
		byHash:          &model.Document{ID: "exist-1", Status: "ready"},
	}
	svc := newIngestSvc(t, rs)

	content := []byte("%PDF-1.4 duplicate content")
	doc, err := svc.Accept(context.Background(), bytes.NewReader(content), int64(len(content)), "dup.pdf", nil)
	if !errors.Is(err, service.ErrDuplicateDocument) {
		t.Fatalf("应返回重复错误, got %v", err)
	}
	if doc == nil || doc.ID != "exist-1" {
		t.Fatalf("应返回已有文档: %+v", doc)
	}
	if len(rs.saved) != 0 {
		t.Fatalf("重复上传不应建新记录, saved = %d", len(rs.saved))
	}
}

// Accept 拒绝非 PDF。
func TestIngestService_AcceptRejectsNonPDF(t *testing.T) {
	rs := &recordStore{rerankMockStore: &rerankMockStore{}}
	svc := newIngestSvc(t, rs)

	if _, err := svc.Accept(context.Background(), bytes.NewReader([]byte("x")), 1, "a.txt", nil); !errors.Is(err, service.ErrUnsupportedType) {
		t.Fatalf("应拒绝非 PDF, got %v", err)
	}
}

// Run 幂等清场：开工先 DeleteChunks（清上次崩溃残留），失败时标记 failed。
func TestIngestService_RunCleansAndFails(t *testing.T) {
	dir := t.TempDir()
	rs := &recordStore{
		rerankMockStore: &rerankMockStore{},
		byID:            &model.Document{ID: "doc-9", Filename: "fake.pdf", Status: "processing", ChunkSize: 800, ChunkOverlap: 150},
	}
	svc := service.NewIngestService(nil, rs,
		config.ServerConfig{UploadDir: dir},
		config.RAGConfig{ChunkSize: 800, ChunkOverlap: 150},
		render.Config{})

	// 落一个假 PDF（内容非法 → 解析失败路径）
	if err := os.WriteFile(filepath.Join(dir, "doc-9.pdf"), []byte("not a real pdf"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := svc.Run(context.Background(), "doc-9")
	if err == nil {
		t.Fatal("假 PDF 应解析失败")
	}
	// 清场调用必须发生在失败标记之前（幂等坑的核心）
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.deleted) != 1 || rs.deleted[0] != "doc-9" {
		t.Fatalf("Run 应先清残留 chunks: %v", rs.deleted)
	}
	if len(rs.saved) == 0 {
		t.Fatal("失败应保存文档状态")
	}
	last := rs.saved[len(rs.saved)-1]
	if last.Status != "failed" || last.Error == "" {
		t.Fatalf("失败应标记 failed + 原因: %+v", last)
	}
}
