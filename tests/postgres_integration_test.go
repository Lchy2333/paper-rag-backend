package tests

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"paper-rag-backend/internal/config"
	"paper-rag-backend/internal/model"
	"paper-rag-backend/internal/store"
)

// TestPostgresStore 是针对真实 PostgreSQL 的集成测试。
// 运行前需设置数据库密码（其他项可用默认值）：
//
//	$env:TEST_DATABASE_PASSWORD = "你的数据库密码"
//	go test ./tests -run TestPostgresStore -v
//
// 未设置该环境变量时测试自动跳过（不影响 go test ./...）。
func TestPostgresStore(t *testing.T) {
	password := os.Getenv("TEST_DATABASE_PASSWORD")
	if password == "" {
		t.Skip("未设置 TEST_DATABASE_PASSWORD，跳过 PostgreSQL 集成测试")
	}

	cfg := config.DatabaseConfig{
		Type:     "postgres",
		Host:     envOr("TEST_DATABASE_HOST", "localhost"),
		Port:     envIntOr("TEST_DATABASE_PORT", 5432),
		User:     envOr("TEST_DATABASE_USER", "postgres"),
		Password: password,
		DBName:   envOr("TEST_DATABASE_DB", "paper_rag"),
		SSLMode:  "disable",
	}

	ctx := context.Background()
	s, err := store.NewPostgresStore(ctx, cfg.DSN(), 5)
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
		ID:         docID,
		Filename:   "integration-test.pdf",
		Title:      "集成测试文档",
		PageCount:  3,
		SizeBytes:  12345,
		ChunkCount: 3,
		Status:     "ready",
		CreatedAt:  time.Now(),
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
	results, err := s.Search(ctx, query, 5, 0.5)
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
	pool, err := pgxpool.New(ctx, cfg.DSN())
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
	after, _ := s.Search(ctx, testVector1024(0), 5, 0)
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

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
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
