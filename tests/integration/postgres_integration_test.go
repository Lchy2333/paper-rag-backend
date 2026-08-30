// Package integration 存放需要真实外部依赖（PostgreSQL）的集成测试。
// 未设置 TEST_DATABASE_PASSWORD 时自动跳过。
package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"paper-rag-backend/internal/config"
	"paper-rag-backend/internal/model"
	"paper-rag-backend/internal/store"
)

// TestPostgresStore 是针对真实 PostgreSQL 的集成测试。
// 数据库配置来自 config/config.test.yaml（默认连 paper_rag_test 测试库）。
// 运行前需设置数据库密码：
//
//	$env:TEST_DATABASE_PASSWORD = "你的数据库密码"
//	go test ./tests/integration -run TestPostgresStore -v
//
// 未设置该环境变量时测试自动跳过（不影响 go test ./...）。
func TestPostgresStore(t *testing.T) {
	cfg, err := config.Load("../../config/config.test.yaml")
	if err != nil {
		t.Fatalf("加载测试配置失败: %v", err)
	}

	// 密码走环境变量（测试 yaml 里是 ${TEST_DATABASE_PASSWORD} 占位符）
	if pw := os.Getenv("TEST_DATABASE_PASSWORD"); pw != "" {
		cfg.Database.Password = pw
	}
	if cfg.Database.Password == "" || strings.HasPrefix(cfg.Database.Password, "${") {
		t.Skip("未设置 TEST_DATABASE_PASSWORD，跳过 PostgreSQL 集成测试")
	}

	ctx := context.Background()
	s, err := store.NewPostgresStore(ctx, cfg.Database.DSN(), cfg.Database.MaxConns)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer s.Close()

	// 测试数据（用时间戳保证唯一，避免污染正式数据）
	ts := time.Now().UnixNano()
	docID := "test-doc-" + itoa(ts)
	chunkIDs := []string{"test-c1-" + itoa(ts), "test-c2-" + itoa(ts), "test-c3-" + itoa(ts)}
	defer cleanupPostgres(ctx, s, docID)

	// 1. 保存文档
	doc := &model.Document{
		ID:          docID,
		UserID:      "local",
		Filename:    "integration-test.pdf",
		Title:       "集成测试文档",
		PageCount:   3,
		SizeBytes:   12345,
		ChunkCount:  3,
		Status:      "ready",
		ContentHash: "testhash-" + itoa(ts),
		CreatedAt:   time.Now(),
	}
	if err := s.SaveDocument(ctx, doc); err != nil {
		t.Fatalf("SaveDocument 失败: %v", err)
	}

	// 2. 查询文档
	got, err := s.GetDocument(ctx, docID)
	if err != nil || got == nil {
		t.Fatalf("GetDocument 失败: %v (got=%v)", err, got)
	}
	if got.Status != "ready" || got.PageCount != 3 {
		t.Fatalf("GetDocument 字段不对: %+v", got)
	}
	if got.ContentHash != doc.ContentHash {
		t.Fatalf("GetDocument 未带回 content_hash: %q", got.ContentHash)
	}
	if got.UserID != "local" {
		t.Fatalf("GetDocument 未带回 user_id: %q", got.UserID)
	}

	// 2.1 按内容哈希查找（上传去重）：命中 / 未命中
	byHash, err := s.FindByContentHash(ctx, doc.ContentHash)
	if err != nil || byHash == nil || byHash.ID != docID {
		t.Fatalf("FindByContentHash 应命中: err=%v byHash=%v", err, byHash)
	}
	miss, err := s.FindByContentHash(ctx, "no-such-hash")
	if err != nil || miss != nil {
		t.Fatalf("FindByContentHash 未命中应返回 nil: err=%v miss=%v", err, miss)
	}

	// 3. 批量写入分块（维度与表定义的 VECTOR(1024) 一致）
	chunks := []model.Chunk{
		{ID: chunkIDs[0], DocumentID: docID, Filename: "integration-test.pdf", Page: 1, Index: 0, Content: "关于向量数据库的原理与应用", Vector: testVector1024(0), CreatedAt: time.Now()},
		{ID: chunkIDs[1], DocumentID: docID, Filename: "integration-test.pdf", Page: 2, Index: 1, Content: "关于深度学习模型的训练方法", Vector: testVector1024(1), CreatedAt: time.Now()},
		{ID: chunkIDs[2], DocumentID: docID, Filename: "integration-test.pdf", Page: 3, Index: 2, Content: "关于股票市场的价格波动规律", Vector: testVector1024(2), CreatedAt: time.Now()},
	}
	if err := s.AddChunks(ctx, chunks); err != nil {
		t.Fatalf("AddChunks 失败: %v", err)
	}

	// 4. 向量检索：查询向量接近 chunk[0]
	query := make([]float32, 1024)
	query[0] = 0.9
	query[1] = 0.1
	results, err := s.Search(ctx, query, 5, 0.5, store.SearchOptions{})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("Search 未返回结果")
	}
	if results[0].Chunk.ID != chunkIDs[0] {
		t.Fatalf("最相似的应为 %s, got %s (score=%v)", chunkIDs[0], results[0].Chunk.ID, results[0].Score)
	}
	if results[0].Chunk.Filename != "integration-test.pdf" {
		t.Fatalf("JOIN documents 未带回 filename: %q", results[0].Chunk.Filename)
	}

	// 4.1 指定文档检索：限定在 docID 范围内，结果应全部来自该文档
	filtered, err := s.Search(ctx, query, 5, 0.5, store.SearchOptions{DocIDs: []string{docID}})
	if err != nil {
		t.Fatalf("Search(限定文档) 失败: %v", err)
	}
	if len(filtered) == 0 {
		t.Fatalf("限定文档检索未返回结果")
	}
	for _, r := range filtered {
		if r.Chunk.DocumentID != docID {
			t.Fatalf("限定文档检索混入了其他文档: %s", r.Chunk.DocumentID)
		}
	}

	// 4.2 指定不存在的文档：应返回空（不报错）
	empty, err := s.Search(ctx, query, 5, 0, store.SearchOptions{DocIDs: []string{"no-such-doc"}})
	if err != nil {
		t.Fatalf("Search(不存在文档) 失败: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("检索不存在文档应返回空, got %d", len(empty))
	}

	// 4.3 用户隔离：限定归属用户检索，本人命中 / 他人应为空
	mine, err := s.Search(ctx, query, 5, 0.5, store.SearchOptions{UserID: "local"})
	if err != nil {
		t.Fatalf("Search(本人 user) 失败: %v", err)
	}
	if len(mine) == 0 {
		t.Fatalf("检索本人 user 的文档应命中")
	}
	others, err := s.Search(ctx, query, 5, 0.5, store.SearchOptions{UserID: "another-user"})
	if err != nil {
		t.Fatalf("Search(他人 user) 失败: %v", err)
	}
	if len(others) != 0 {
		t.Fatalf("检索他人 user 应返回空, got %d", len(others))
	}

	// 5. 列表
	docs, err := s.ListDocuments(ctx)
	if err != nil {
		t.Fatalf("ListDocuments 失败: %v", err)
	}
	found := false
	for _, d := range docs {
		if d.ID == docID {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListDocuments 中找不到测试文档")
	}

	// 5.1 NULL 字段兼容：手工插入 error/title 为 NULL 的行，列表不应报错
	// （黑盒测试无法访问 store 内部的连接池，这里自建一条连接执行裸 SQL）
	pool, err := pgxpool.New(ctx, cfg.Database.DSN())
	if err != nil {
		t.Fatalf("自建连接池失败: %v", err)
	}
	nullDocID := "test-null-" + itoa(ts)
	_, err = pool.Exec(ctx, `INSERT INTO documents (id, filename, status) VALUES ($1, 'null.pdf', 'ready')`, nullDocID)
	pool.Close()
	if err != nil {
		t.Fatalf("插入 NULL 测试行失败: %v", err)
	}
	docs2, err := s.ListDocuments(ctx)
	if err != nil {
		t.Fatalf("ListDocuments 遇到 NULL 字段报错: %v", err)
	}
	for _, d := range docs2 {
		if d.ID == nullDocID {
			if d.Title != "" || d.Error != "" {
				t.Fatalf("NULL 字段应被 COALESCE 为空串, got title=%q error=%q", d.Title, d.Error)
			}
		}
	}
	if err := s.DeleteDocument(ctx, nullDocID); err != nil {
		t.Fatalf("清理 NULL 测试行失败: %v", err)
	}

	// 6. 删除文档（应级联删除 chunks）
	if err := s.DeleteDocument(ctx, docID); err != nil {
		t.Fatalf("DeleteDocument 失败: %v", err)
	}
	got2, _ := s.GetDocument(ctx, docID)
	if got2 != nil {
		t.Fatalf("删除后文档仍存在")
	}
	after, _ := s.Search(ctx, testVector1024(0), 5, 0, store.SearchOptions{})
	for _, r := range after {
		if r.Chunk.DocumentID == docID {
			t.Fatalf("删除文档后 chunks 未级联清理: %s", r.Chunk.ID)
		}
	}

	t.Log("PostgreSQL 集成测试全部通过 ✔")
}

// testVector1024 生成长度 1024 的测试向量，指定位置为 1，其余为 0。
func testVector1024(pos int) []float32 {
	v := make([]float32, 1024)
	v[pos] = 1
	return v
}

func cleanupPostgres(ctx context.Context, s store.Store, docID string) {
	_ = s.DeleteDocument(ctx, docID)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
