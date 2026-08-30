package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"

	"paper-rag-backend/internal/model"
)

// PostgresStore 是基于 PostgreSQL + pgvector 的 Store 实现，
// 对应数据库 paper_rag 中的 documents / chunks 两张表。
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore 连接数据库并返回存储实现。
// dsn 形如 postgres://user:pass@host:5432/dbname?sslmode=disable
func NewPostgresStore(ctx context.Context, dsn string, maxConns int) (*PostgresStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("解析数据库连接串失败: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = int32(maxConns)
	}
	// 每个新连接上注册 pgvector 类型，使 pgx 能编解码 VECTOR 列
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, conn)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("创建数据库连接池失败: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("数据库连接失败（请确认 PostgreSQL 服务已启动）: %w", err)
	}
	return &PostgresStore{pool: pool}, nil
}

// Close 关闭连接池。
func (s *PostgresStore) Close() {
	s.pool.Close()
}

// ---- 文档元数据 ----

func (s *PostgresStore) SaveDocument(ctx context.Context, doc *model.Document) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO documents (id, filename, title, page_count, size_bytes, chunk_count, status, error, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (id) DO UPDATE SET
			filename = EXCLUDED.filename,
			title = EXCLUDED.title,
			page_count = EXCLUDED.page_count,
			size_bytes = EXCLUDED.size_bytes,
			chunk_count = EXCLUDED.chunk_count,
			status = EXCLUDED.status,
			error = EXCLUDED.error`,
		doc.ID, doc.Filename, doc.Title, doc.PageCount, doc.SizeBytes,
		doc.ChunkCount, doc.Status, doc.Error, doc.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("保存文档失败: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetDocument(ctx context.Context, id string) (*model.Document, error) {
	var d model.Document
	err := s.pool.QueryRow(ctx, `
		SELECT id, filename, COALESCE(title, ''), page_count, size_bytes, chunk_count, status, COALESCE(error, ''), created_at
		FROM documents WHERE id = $1`, id,
	).Scan(&d.ID, &d.Filename, &d.Title, &d.PageCount, &d.SizeBytes,
		&d.ChunkCount, &d.Status, &d.Error, &d.CreatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("查询文档失败: %w", err)
	}
	return &d, nil
}

func (s *PostgresStore) ListDocuments(ctx context.Context) ([]model.Document, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, filename, COALESCE(title, ''), page_count, size_bytes, chunk_count, status, COALESCE(error, ''), created_at
		FROM documents ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("查询文档列表失败: %w", err)
	}
	defer rows.Close()

	docs := make([]model.Document, 0)
	for rows.Next() {
		var d model.Document
		if err := rows.Scan(&d.ID, &d.Filename, &d.Title, &d.PageCount, &d.SizeBytes,
			&d.ChunkCount, &d.Status, &d.Error, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("读取文档列表失败: %w", err)
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// DeleteDocument 删除文档，其 chunks 由外键 ON DELETE CASCADE 自动级联删除。
func (s *PostgresStore) DeleteDocument(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM documents WHERE id = $1`, id); err != nil {
		return fmt.Errorf("删除文档失败: %w", err)
	}
	return nil
}

// ---- 向量 ----

// AddChunks 批量写入分块（含向量）。使用事务保证要么全部成功要么全部回滚。
func (s *PostgresStore) AddChunks(ctx context.Context, chunks []model.Chunk) error {
	if len(chunks) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback(ctx)

	for _, c := range chunks {
		_, err := tx.Exec(ctx, `
			INSERT INTO chunks (id, document_id, page, idx, content, embedding, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			c.ID, c.DocumentID, c.Page, c.Index, c.Content,
			pgvector.NewVector(c.Vector), c.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("写入分块失败: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// Search 用余弦相似度检索最相近的 topK 个片段，并按 threshold 过滤。
// docIDs 可选：传入时仅在指定文档范围内检索（WHERE document_id = ANY）。
// 通过 JOIN documents 带回文件名用于引用展示。
func (s *PostgresStore) Search(ctx context.Context, query []float32, topK int, threshold float32, docIDs ...string) ([]SearchResult, error) {
	if topK <= 0 {
		topK = 5
	}

	sql := `
		SELECT c.id, c.document_id, d.filename, c.page, c.idx, c.content, c.created_at,
			   1 - (c.embedding <=> $1) AS similarity
		FROM chunks c
		JOIN documents d ON c.document_id = d.id`
	args := []any{pgvector.NewVector(query)}
	limitPos := 2
	if len(docIDs) > 0 {
		sql += `
		WHERE c.document_id = ANY($2)`
		args = append(args, docIDs)
		limitPos = 3
	}
	sql += fmt.Sprintf(`
		ORDER BY c.embedding <=> $1
		LIMIT $%d`, limitPos)
	args = append(args, topK)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("向量检索失败: %w", err)
	}
	defer rows.Close()

	results := make([]SearchResult, 0, topK)
	for rows.Next() {
		var c model.Chunk
		var score float32
		if err := rows.Scan(&c.ID, &c.DocumentID, &c.Filename, &c.Page, &c.Index,
			&c.Content, &c.CreatedAt, &score); err != nil {
			return nil, fmt.Errorf("读取检索结果失败: %w", err)
		}
		if score < threshold {
			continue
		}
		results = append(results, SearchResult{Chunk: c, Score: score})
	}
	return results, rows.Err()
}

var _ Store = (*PostgresStore)(nil)
