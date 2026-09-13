package store

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"

	"paper-rag-backend/internal/logger"
	"paper-rag-backend/internal/model"
	"paper-rag-backend/internal/segment"
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
		INSERT INTO documents (id, user_id, filename, title, page_count, size_bytes, chunk_count, status, error, content_hash, chunk_size, chunk_overlap, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (id) DO UPDATE SET
			filename = EXCLUDED.filename,
			title = EXCLUDED.title,
			page_count = EXCLUDED.page_count,
			size_bytes = EXCLUDED.size_bytes,
			chunk_count = EXCLUDED.chunk_count,
			status = EXCLUDED.status,
			error = EXCLUDED.error,
			chunk_size = EXCLUDED.chunk_size,
			chunk_overlap = EXCLUDED.chunk_overlap`,
		doc.ID, doc.UserID, doc.Filename, doc.Title, doc.PageCount, doc.SizeBytes,
		doc.ChunkCount, doc.Status, doc.Error, doc.ContentHash, doc.ChunkSize, doc.ChunkOverlap, doc.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("保存文档失败: %w", err)
	}
	return nil
}

// docColumns 是文档表的查询列（含分块参数），各查询复用。
const docColumns = `id, user_id, filename, COALESCE(title, ''), page_count, size_bytes, chunk_count, status, COALESCE(error, ''), COALESCE(content_hash, ''), chunk_size, chunk_overlap, created_at`

func (s *PostgresStore) GetDocument(ctx context.Context, id string) (*model.Document, error) {
	var d model.Document
	err := s.pool.QueryRow(ctx, `
		SELECT `+docColumns+`
		FROM documents WHERE id = $1`, id,
	).Scan(&d.ID, &d.UserID, &d.Filename, &d.Title, &d.PageCount, &d.SizeBytes,
		&d.ChunkCount, &d.Status, &d.Error, &d.ContentHash, &d.ChunkSize, &d.ChunkOverlap, &d.CreatedAt)
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
		SELECT `+docColumns+`
		FROM documents ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("查询文档列表失败: %w", err)
	}
	defer rows.Close()

	docs := make([]model.Document, 0)
	for rows.Next() {
		var d model.Document
		if err := rows.Scan(&d.ID, &d.UserID, &d.Filename, &d.Title, &d.PageCount, &d.SizeBytes,
			&d.ChunkCount, &d.Status, &d.Error, &d.ContentHash, &d.ChunkSize, &d.ChunkOverlap, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("读取文档列表失败: %w", err)
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// FindByContentHash 按内容哈希查找文档（上传去重用），找不到返回 nil。
func (s *PostgresStore) FindByContentHash(ctx context.Context, hash string) (*model.Document, error) {
	var d model.Document
	err := s.pool.QueryRow(ctx, `
		SELECT `+docColumns+`
		FROM documents WHERE content_hash = $1`, hash,
	).Scan(&d.ID, &d.UserID, &d.Filename, &d.Title, &d.PageCount, &d.SizeBytes,
		&d.ChunkCount, &d.Status, &d.Error, &d.ContentHash, &d.ChunkSize, &d.ChunkOverlap, &d.CreatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("按内容哈希查询文档失败: %w", err)
	}
	return &d, nil
}

// DeleteDocument 删除文档，其 chunks 由外键 ON DELETE CASCADE 自动级联删除。
func (s *PostgresStore) DeleteDocument(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM documents WHERE id = $1`, id); err != nil {
		return fmt.Errorf("删除文档失败: %w", err)
	}
	return nil
}

// DeleteChunks 删除某文档的全部 chunk（重分块用），文档元数据保持不变。
func (s *PostgresStore) DeleteChunks(ctx context.Context, documentID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM chunks WHERE document_id = $1`, documentID); err != nil {
		return fmt.Errorf("删除分块失败: %w", err)
	}
	return nil
}

// ---- 向量 ----

// AddChunks 批量写入分块（含向量与分词 tsvector）。使用事务保证要么全部成功要么全部回滚。
// tsv 由 Go 侧 gojieba 分词后经 to_tsvector('simple') 生成（simple 配置不分词，
// 保留已分好的词；中文分词在应用层完成，绕开 PostgreSQL 无中文分词器的问题）。
func (s *PostgresStore) AddChunks(ctx context.Context, chunks []model.Chunk) error {
	if len(chunks) == 0 {
		return nil
	}

	start := time.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback(ctx)

	for _, c := range chunks {
		tokens := segment.ToSearchString(c.Content)
		_, err := tx.Exec(ctx, `
			INSERT INTO chunks (id, document_id, page, idx, content, embedding, tsv, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, to_tsvector('simple', $7), $8)`,
			c.ID, c.DocumentID, c.Page, c.Index, c.Content,
			pgvector.NewVector(c.Vector), tokens, c.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("写入分块失败: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	logger.Debug("store.add_chunks_done", "chunks", len(chunks), "duration_ms", time.Since(start).Milliseconds())
	return nil
}

// Search 返回与 query 最相似的 topK 个片段。
//
// 默认走两阶段向量检索：
//
//	阶段一（粗筛）：开启 pgvector iterative_scan（relaxed_order），LIMIT 放宽到
//	                max(topK*coarseFactor, coarseMin)，不带过滤条件，纯向量排序多捞候选。
//	                目的是让 HNSW 尽量扫到更多节点，避免过滤条件下候选不足。
//	阶段二（精筛+补漏）：应用层按 opts（DocIDs/UserID）过滤粗筛结果；若过滤后不足
//	                topK，用合规 DocIDs 走 document_id B-tree 索引精确取回补齐。
//
// 若 opts.UseHybrid=true，额外走字面检索（trigram 子串匹配，对 QueryText 拆词），
// 两路结果用 RRF（倒数排名融合）合并——解决公式/编号/专名这类字面精确匹配场景
// （embedding 擅长主题语义、不擅长符号精确匹配，见 避坑指南 #7）。
//
// 返回结果去重（document_id+page+idx）、截断 topK、并按 threshold 过滤。
func (s *PostgresStore) Search(ctx context.Context, query []float32, topK int, threshold float32, opts SearchOptions) ([]SearchResult, error) {
	if topK <= 0 {
		topK = 5
	}

	// 向量路
	searchStart := time.Now()
	vectorResults, err := s.searchVector(ctx, query, topK, threshold, opts)
	if err != nil {
		return nil, err
	}
	logger.Debug("search.vector_done", "hits", len(vectorResults), "duration_ms", time.Since(searchStart).Milliseconds())

	// 非混合：直接返回向量结果
	if !opts.UseHybrid || strings.TrimSpace(opts.QueryText) == "" {
		return vectorResults, nil
	}

	// 混合：向量路 + tsvector 全文路（主力，真 BM25 风格）+ pg_trgm 字面路（补充公式/符号）→ RRF 合并
	tsvResults, err := s.searchTsvector(ctx, opts.QueryText, topK, opts)
	if err != nil {
		// 全文路失败不阻塞：降级到 pg_trgm 字面路。这里必须打 warn——
		// 失败被吞掉会让结果悄悄变差且无从察觉，warn 暴露降级。
		logger.Warn("search.tsv_failed", "err", err)
		tsvResults = nil
	} else {
		logger.Debug("search.tsv_done", "hits", len(tsvResults))
	}

	lexResults, err := s.searchLexical(ctx, opts.QueryText, topK, opts)
	if err != nil {
		logger.Warn("search.lex_failed", "err", err)
		lexResults = nil
	} else {
		logger.Debug("search.lex_done", "hits", len(lexResults))
	}

	merged := rrfMerge(vectorResults, tsvResults, topK, threshold)
	merged = rrfMerge(merged, lexResults, topK, threshold)
	logger.Debug("search.merged_done", "hits", len(merged), "top_k", topK)
	return merged, nil
}

// searchVector 实现两阶段向量检索（粗筛 + 精筛 + 补漏）。
func (s *PostgresStore) searchVector(ctx context.Context, query []float32, topK int, threshold float32, opts SearchOptions) ([]SearchResult, error) {
	// ---- 阶段一：粗筛（放宽 LIMIT + iterative_scan，不带过滤条件） ----
	const coarseFactor = 20
	const coarseMin = 100
	coarseLimit := topK * coarseFactor
	if coarseLimit < coarseMin {
		coarseLimit = coarseMin
	}

	// iterative_scan 是会话级参数，pgxpool 需要独占连接来设置。
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Release()

	// pgvector 0.8+：relaxed_order 允许 HNSW 在过滤/放宽场景下返回未精确排序的
	// 近似结果，从而在同样的 ef_search 下扫描更多节点（提高召回）。粗筛不过滤，
	// 该设置仍有助于在 LIMIT 较大时尽快返回足够多的近邻。
	if _, err := conn.Exec(ctx, "SET hnsw.iterative_scan = relaxed_order"); err != nil {
		return nil, fmt.Errorf("设置 iterative_scan 失败: %w", err)
	}

	coarseSQL := `
		SELECT c.id, c.document_id, d.filename, d.user_id, c.page, c.idx, c.content, c.created_at,
			   1 - (c.embedding <=> $1) AS similarity
		FROM chunks c
		JOIN documents d ON c.document_id = d.id
		ORDER BY c.embedding <=> $1
		LIMIT $2`
	coarse, err := queryChunks(ctx, conn, coarseSQL, pgvector.NewVector(query), coarseLimit)
	if err != nil {
		return nil, fmt.Errorf("粗筛检索失败: %w", err)
	}

	// ---- 阶段二：应用层精筛 ----
	filtered := make([]SearchResult, 0, len(coarse))
	seen := make(map[string]struct{}, len(coarse))
	for _, r := range coarse {
		if !matchOptions(r.Chunk, opts) {
			continue
		}
		key := chunkKey(r.Chunk)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		filtered = append(filtered, r)
		if len(filtered) >= topK {
			break
		}
	}

	// ---- 补漏：精筛不足 topK 时，用合规 DocIDs 精确取回 ----
	if len(filtered) < topK && len(opts.DocIDs) > 0 {
		docIDs := opts.DocIDs
		// 若已限定 UserID，过滤出属于该用户的合规文档（一次查询避免 N+1）
		if opts.UserID != "" {
			docIDs = s.filterDocsByUser(ctx, conn, docIDs, opts.UserID)
		}
		if len(docIDs) > 0 {
			fillSQL := `
				SELECT c.id, c.document_id, d.filename, d.user_id, c.page, c.idx, c.content, c.created_at,
					   1 - (c.embedding <=> $1) AS similarity
				FROM chunks c
				JOIN documents d ON c.document_id = d.id
				WHERE c.document_id = ANY($2)
				ORDER BY c.embedding <=> $1
				LIMIT $3`
			fill, err := queryChunks(ctx, conn, fillSQL, pgvector.NewVector(query), docIDs, topK)
			if err != nil {
				return nil, fmt.Errorf("补漏检索失败: %w", err)
			}
			for _, r := range fill {
				key := chunkKey(r.Chunk)
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				filtered = append(filtered, r)
				if len(filtered) >= topK {
					break
				}
			}
		}
	}

	// ---- 排序 + threshold 过滤 ----
	results := make([]SearchResult, 0, len(filtered))
	for _, r := range filtered {
		if r.Score < threshold {
			continue
		}
		results = append(results, r)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if len(results) > topK {
		results = results[:topK]
	}
	return results, nil
}

// searchLexical 对查询文本拆词，用 trigram 子串匹配检索 content 含这些词的 chunk。
// 用 pg_trgm 的 similarity() 排序：既过滤（% 匹配）又按字面相似度排名。
// 按 opts 应用 user_id / document_id 过滤（与向量路一致）。
func (s *PostgresStore) searchLexical(ctx context.Context, queryText string, topK int, opts SearchOptions) ([]SearchResult, error) {
	terms := lexTerms(queryText)
	if len(terms) == 0 {
		return nil, nil
	}

	// 用整句查询做 similarity 排序（pg_trgm 支持，走 GIN 索引），
	// 同时用拆出的词做 ILIKE 过滤，兼顾召回与排序。
	// 多个词之间用 OR（词越多召回越大），归属过滤用 AND 追加。
	var cond []string
	args := []any{}
	termConds := make([]string, 0, len(terms))
	for i, t := range terms {
		termConds = append(termConds, fmt.Sprintf("c.content ILIKE $%d", i+1))
		args = append(args, "%"+t+"%")
	}
	if len(termConds) > 0 {
		cond = append(cond, "("+strings.Join(termConds, " OR ")+")")
	}

	// 归属过滤（user_id / document_id），用 AND 追加在词条件之后
	filterClause, filterArgs := buildFilterClause(opts)
	if filterClause != "" {
		cond = append(cond, filterClause)
		args = append(args, filterArgs...)
	}
	simPos := len(args) + 1
	limitPos := simPos + 1
	args = append(args, queryText, topK*5)

	sql := `
		SELECT c.id, c.document_id, d.filename, d.user_id, c.page, c.idx, c.content, c.created_at,
			   similarity(c.content, $` + itoa(simPos) + `) AS similarity
		FROM chunks c
		JOIN documents d ON c.document_id = d.id
		WHERE ` + strings.Join(cond, " AND ") + `
		ORDER BY similarity DESC
		LIMIT $` + itoa(limitPos)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("字面检索失败: %w", err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var c model.Chunk
		var score float32
		if err := rows.Scan(&c.ID, &c.DocumentID, &c.Filename, &c.UserID, &c.Page, &c.Index,
			&c.Content, &c.CreatedAt, &score); err != nil {
			return nil, err
		}
		out = append(out, SearchResult{Chunk: c, Score: score})
	}
	return out, rows.Err()
}

// searchTsvector 用 tsvector 全文检索（主力，真 BM25 风格排序）。
//
// 分词在 Go 侧完成（gojieba，见 internal/segment），查询文本分词后拼成
// 空格分隔串，用 websearch_to_tsquery('simple', $q) 构造查询——simple 配置
// 不额外分词，直接使用传入的词；空格在 websearch 语法里表示 AND。
// 排序用 ts_rank（词频+逆文档频率+长度归一，类 BM25）。
//
// 用 ts_rank 的两个权重参数（默认 A/B/C/D = 0.1/0.2/0.4/1.0），
// 命中标题/正文的权重可后续按需调整。
func (s *PostgresStore) searchTsvector(ctx context.Context, queryText string, topK int, opts SearchOptions) ([]SearchResult, error) {
	tokens := segment.ToSearchString(queryText)
	if strings.TrimSpace(tokens) == "" {
		return nil, nil
	}

	// 公式/专名整串保留 + 中文分词词，用 AND 组合保证精确（websearch 空格=AND）
	// 若 AND 无结果可降级 OR，v1 先用 AND + 加大召回 LIMIT。
	query := strings.ReplaceAll(tokens, " ", " & ")
	tsq := fmt.Sprintf("to_tsquery('simple', %q)", query)

	// 归属过滤（user_id / document_id）
	filterClause, filterArgs := buildFilterClause(opts)
	where := "c.tsv @@ " + tsq
	if filterClause != "" {
		where += " AND " + filterClause
	}
	limitPos := len(filterArgs) + 1
	args := append(filterArgs, topK*5)

	sql := `
		SELECT c.id, c.document_id, d.filename, d.user_id, c.page, c.idx, c.content, c.created_at,
			   ts_rank(c.tsv, ` + tsq + `) AS similarity
		FROM chunks c
		JOIN documents d ON c.document_id = d.id
		WHERE ` + where + `
		ORDER BY similarity DESC
		LIMIT $` + itoa(limitPos)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("全文检索失败: %w", err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var c model.Chunk
		var score float32
		if err := rows.Scan(&c.ID, &c.DocumentID, &c.Filename, &c.UserID, &c.Page, &c.Index,
			&c.Content, &c.CreatedAt, &score); err != nil {
			return nil, err
		}
		out = append(out, SearchResult{Chunk: c, Score: score})
	}
	return out, rows.Err()
}
//
//	score(chunk) = Σ_{列表} 1 / (k + rank)，k 为常数（默认 60）。
//
// 两条路都命中的 chunk 排名靠前；单路命中的按排名贡献。合并后按 score 降序、
// 去重、截断 topK，并按 threshold 过滤（RRF 分数为加和，threshold 通常传 0）。
func rrfMerge(vector, lexical []SearchResult, topK int, threshold float32) []SearchResult {
	const k = 60.0
	scores := make(map[string]float64)
	chunks := make(map[string]model.Chunk)

	rankScore := func(list []SearchResult) {
		for i, r := range list {
			key := chunkKey(r.Chunk)
			scores[key] += 1.0 / (k + float64(i+1))
			if _, ok := chunks[key]; !ok {
				chunks[key] = r.Chunk
			}
		}
	}
	rankScore(vector)
	rankScore(lexical)

	type scored struct {
		key   string
		score float64
	}
	all := make([]scored, 0, len(scores))
	for key, sc := range scores {
		all = append(all, scored{key, sc})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })

	out := make([]SearchResult, 0, topK)
	for _, item := range all {
		if float32(item.score) < threshold {
			continue
		}
		out = append(out, SearchResult{Chunk: chunks[item.key], Score: float32(item.score)})
		if len(out) >= topK {
			break
		}
	}
	return out
}

// lexTerms 把查询文本拆成检索词：按空白/标点拆分，过滤过短/纯符号词。
// 公式类查询（如 "V=0.5πtf'Dcotθ"）整串保留为一个词，便于字面精确匹配。
func lexTerms(query string) []string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '，' || r == '。' || r == '？' ||
			r == '！' || r == '；' || r == '、' || r == '"' || r == '“' || r == '”'
	})
	var out []string
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if len([]rune(f)) < 2 {
			continue
		}
		out = append(out, f)
	}
	return out
}

// chunkKey 返回 chunk 去重键（document_id + page + idx）。
func chunkKey(c model.Chunk) string {
	return fmt.Sprintf("%s:%d:%d", c.DocumentID, c.Page, c.Index)
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

// matchOptions 判断 chunk 是否满足检索过滤条件。
func matchOptions(c model.Chunk, opts SearchOptions) bool {
	if opts.UserID != "" && c.UserID != opts.UserID {
		return false
	}
	if len(opts.DocIDs) > 0 {
		ok := false
		for _, id := range opts.DocIDs {
			if c.DocumentID == id {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// filterDocsByUser 从 docIDs 中过滤出属于指定用户的文档。
func (s *PostgresStore) filterDocsByUser(ctx context.Context, conn *pgxpool.Conn, docIDs []string, userID string) []string {
	rows, err := conn.Query(ctx, `SELECT id FROM documents WHERE id = ANY($1) AND user_id = $2`, docIDs, userID)
	if err != nil {
		return docIDs // 查询失败时退回不过滤，交给上层兜底
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return docIDs
		}
		out = append(out, id)
	}
	return out
}

// buildFilterClause 生成检索过滤条件（user_id / document_id）。
// 返回 SQL 片段（不含 WHERE 关键字）与对应参数，供全文路/字面路复用，
// 保证和向量路一致地遵守用户归属与文档范围。
func buildFilterClause(opts SearchOptions) (string, []any) {
	var conds []string
	var args []any
	if opts.UserID != "" {
		args = append(args, opts.UserID)
		conds = append(conds, fmt.Sprintf("d.user_id = $%d", len(args)))
	}
	if len(opts.DocIDs) > 0 {
		args = append(args, opts.DocIDs)
		conds = append(conds, fmt.Sprintf("c.document_id = ANY($%d)", len(args)))
	}
	return strings.Join(conds, " AND "), args
}

// queryChunks 执行一段 chunk 检索 SQL（统一扫描结果行）。
func queryChunks(ctx context.Context, conn *pgxpool.Conn, sql string, args ...any) ([]SearchResult, error) {
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		var c model.Chunk
		var score float32
		if err := rows.Scan(&c.ID, &c.DocumentID, &c.Filename, &c.UserID, &c.Page, &c.Index,
			&c.Content, &c.CreatedAt, &score); err != nil {
			return nil, err
		}
		out = append(out, SearchResult{Chunk: c, Score: score})
	}
	return out, rows.Err()
}

var _ Store = (*PostgresStore)(nil)
