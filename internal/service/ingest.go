package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"paper-rag-backend/internal/ai"
	"paper-rag-backend/internal/chunker"
	"paper-rag-backend/internal/config"
	"paper-rag-backend/internal/logger"
	"paper-rag-backend/internal/model"
	"paper-rag-backend/internal/parser"
	"paper-rag-backend/internal/render"
	"paper-rag-backend/internal/store"
)

// IngestService 负责文档的摄入流水线：落盘 → 解析 → 分块 → 向量化 → 入库。
type IngestService struct {
	ai        *ai.Client
	store     store.Store
	chunkCfg  chunker.ChunkConfig
	uploadDir string
	renderCfg render.Config
}

// NewIngestService 创建文档摄入服务。
func NewIngestService(c *ai.Client, s store.Store, cfg config.ServerConfig, rag config.RAGConfig, renderCfg render.Config) *IngestService {
	return &IngestService{
		ai:        c,
		store:     s,
		chunkCfg:  chunker.ChunkConfig{Size: rag.ChunkSize, Overlap: rag.ChunkOverlap},
		uploadDir: cfg.UploadDir,
		renderCfg: renderCfg,
	}
}

// defaultOwnerUserID 已移除（2026-10）：多用户后文档归属一律由 X-User-ID 头
// 经 handler 显式传入，不再有单用户兜底值。admin 为超级用户（作用域空 = 全库）。

// ErrUnsupportedType 表示上传的文件类型不受支持。
var ErrUnsupportedType = errors.New("不支持的文件类型，仅支持 PDF")

// ErrDuplicateDocument 表示上传的文件内容与库中已有文档相同（内容哈希一致）。
var ErrDuplicateDocument = errors.New("文档已存在（内容相同），已跳过重复上传")

// ErrDocumentNotFound 表示按 ID 查不到文档（重分块/查询时）。
var ErrDocumentNotFound = errors.New("文档不存在")

// resolveChunkConfig 把请求携带的文档级分块参数解析为实际配置。
// opts 为 nil 或字段为 nil 时回退到 base（全局默认/文档当前值）；参数非法时返回错误。
func resolveChunkConfig(base chunker.ChunkConfig, opts *model.IngestOptions) (chunker.ChunkConfig, error) {
	if opts == nil {
		return base, nil
	}
	cfg := base
	if opts.ChunkSize != nil {
		if *opts.ChunkSize < 1 {
			return cfg, fmt.Errorf("chunk_size 必须为正整数")
		}
		cfg.Size = *opts.ChunkSize
	}
	if opts.ChunkOverlap != nil {
		if *opts.ChunkOverlap < 0 {
			return cfg, fmt.Errorf("chunk_overlap 不能为负数")
		}
		if *opts.ChunkOverlap >= cfg.Size {
			return cfg, fmt.Errorf("chunk_overlap (%d) 必须小于 chunk_size (%d)", *opts.ChunkOverlap, cfg.Size)
		}
		cfg.Overlap = *opts.ChunkOverlap
	}
	return cfg, nil
}

// Rechunk 删除文档的全部 chunk 后，用新的分块参数重新解析、分块、向量化并入库。
// userID 为 "" 表示 admin 超级用户（不过滤归属）。
// opts 为 nil 或字段为空时回退到文档当前使用的参数，再回退全局默认。
// 失败时不删除已有 chunk（保持旧数据可用），仅返回错误。
func (s *IngestService) Rechunk(ctx context.Context, id string, userID string, opts *model.IngestOptions) (*model.Document, error) {
	doc, err := s.store.GetDocument(ctx, id, userID)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, ErrDocumentNotFound
	}
	if doc.Status == "failed" {
		return nil, fmt.Errorf("文档解析失败，无法重新分块")
	}

	diskPath := filepath.Join(s.uploadDir, doc.ID+".pdf")
	f, err := os.Open(diskPath)
	if err != nil {
		return nil, fmt.Errorf("原始 PDF 文件不存在（%v）", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	l := logger.With("doc_id", doc.ID, "file", doc.Filename)

	// 分块参数优先级：请求 > 文档当前值 > 全局默认
	base := chunker.ChunkConfig{Size: doc.ChunkSize, Overlap: doc.ChunkOverlap}
	if base.Size <= 0 {
		base = s.chunkCfg
	}
	cfg, err := resolveChunkConfig(base, opts)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	l.Info("rechunk.start", "old_chunks", doc.ChunkCount, "old_chunk_size", doc.ChunkSize, "new_chunk_size", cfg.Size, "new_chunk_overlap", cfg.Overlap)

	models, err := s.buildChunkModels(ctx, f, fi.Size(), diskPath, doc.Filename, cfg, doc, l)
	if err != nil {
		// 重分块失败：不动已有 chunks，只报错
		return doc, fmt.Errorf("重新分块失败: %w", err)
	}

	// 删旧 → 写新 → 更新文档元数据
	if err := s.store.DeleteChunks(ctx, doc.ID); err != nil {
		l.Error("store.delete_chunks_failed", "err", err)
		return doc, fmt.Errorf("删除旧 chunk 失败: %w", err)
	}
	if err := s.store.AddChunks(ctx, models); err != nil {
		l.Error("store.add_chunks_failed", "chunks", len(models), "err", err)
		return doc, fmt.Errorf("写入新 chunk 失败: %w", err)
	}
	doc.ChunkCount = len(models)
	doc.ChunkSize = cfg.Size
	doc.ChunkOverlap = cfg.Overlap
	doc.Status = "ready"
	doc.Error = ""
	if err := s.store.SaveDocument(ctx, doc); err != nil {
		return doc, err
	}
	l.Info("rechunk.done", "chunks", doc.ChunkCount, "duration_ms", time.Since(start).Milliseconds())
	return doc, nil
}

// Accept 接收一个上传文件：校验 → 落盘 → 去重 → 建 pending 记录（即入队）。
// 是摄入流水线的同步段：只做快活（毫秒级），慢活（解析/OCR/向量化）全部留给 worker 异步执行。
// 处理进度由前端轮询 GET /documents/:id 的 status 字段（pending → processing → ready/failed）。
// opts 可携带文档级分块参数（chunk_size/chunk_overlap），nil 字段回退到 config.yaml 的 rag 默认。
// userID 由 handler 从 X-User-ID 头传入（必非空），作为文档归属。
func (s *IngestService) Accept(ctx context.Context, file io.ReaderAt, size int64, filename string, userID string, opts *model.IngestOptions) (*model.Document, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext != ".pdf" {
		return nil, ErrUnsupportedType
	}

	if err := os.MkdirAll(s.uploadDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建上传目录失败: %w", err)
	}

	// 解析文档级分块参数（缺省用全局默认）；校验失败直接拒绝（用户错误当场反馈）
	cfg, err := resolveChunkConfig(s.chunkCfg, opts)
	if err != nil {
		return nil, err
	}

	doc := &model.Document{
		ID:           newID(),
		UserID:       userID,
		Filename:     filename,
		Status:       "pending",
		SizeBytes:    size,
		ChunkSize:    cfg.Size,
		ChunkOverlap: cfg.Overlap,
		CreatedAt:    time.Now(),
	}

	// 任务级 logger：本文档所有阶段日志都带 doc_id，跨同步/异步两段串联整条链路
	l := logger.With("doc_id", doc.ID, "file", filename)
	start := time.Now()
	l.Info("ingest.accept", "size_bytes", size, "chunk_size", cfg.Size, "chunk_overlap", cfg.Overlap)

	// 1. 落盘保存原始文件，同时计算内容哈希（SHA-256）用于去重
	diskPath := filepath.Join(s.uploadDir, doc.ID+ext)
	f, err := os.Create(diskPath)
	if err != nil {
		return nil, fmt.Errorf("保存上传文件失败: %w", err)
	}
	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, h), io.NewSectionReader(file, 0, size))
	cerr := f.Close()
	if err != nil {
		return nil, fmt.Errorf("写入上传文件失败: %w", err)
	}
	if cerr != nil {
		return nil, cerr
	}
	_ = written
	doc.ContentHash = hex.EncodeToString(h.Sum(nil))

	// 1.5 去重：内容哈希已存在则跳过（删除刚落盘的文件）。
	// 注意：仅当已有文档不是 failed 时才视为重复——失败文档说明内容从未入库，
	// 允许重新上传重试。去重留在同步段，重复当场反馈、不入队。
	// ⚠️ 已知遗留（2026-10，见避坑 #14）："先查后插"非原子 + status!='failed' 判据过窄——
	//   ① 两请求并发传同内容 → 第二个撞唯一索引报 500；② 卡在 pending/processing 的文档
	//   chunk_count=0 却被判重复。待办：store.ClaimUpload（事务+咨询锁+chunk_count 判据）。
	existing, err := s.store.FindByContentHash(ctx, doc.ContentHash, userID)
	if err != nil {
		return nil, fmt.Errorf("检查重复文档失败: %w", err)
	}
	if existing != nil && existing.Status != "failed" {
		_ = os.Remove(diskPath)
		l.Info("ingest.duplicate", "existing_doc_id", existing.ID)
		return existing, ErrDuplicateDocument
	}

	// 2. 建 pending 记录 = 入队（worker 的 ClaimPendingTask 靠 status='pending' 抢单）
	if err := s.store.SaveDocument(ctx, doc); err != nil {
		l.Error("store.save_document_failed", "err", err)
		return nil, fmt.Errorf("保存文档记录失败: %w", err)
	}
	l.Info("ingest.queued", "duration_ms", time.Since(start).Milliseconds())
	return doc, nil
}

// Run 执行摄入流水线的异步段：从磁盘读 PDF → 清残留 → 解析/分块/公式OCR/向量化 → 入库。
// 由 worker 在 ClaimPendingTask 抢单后调用（此时 status=processing）。
// ctx 是 worker 给的任务级 context（不随上传请求断连取消）。
//
// 幂等性：开工先无条件 DeleteChunks 清场——上次崩溃可能停在
// "AddChunks 已写库、status 还没改 ready" 的中间态，重跑直接 AddChunks 会插入重复 chunk
// （chunk 主键是新 ID，无唯一约束拦不住）。删旧重写后，本函数可安全重入。
func (s *IngestService) Run(ctx context.Context, docID string) error {
	// worker 内部操作（摄入流水线异步段），不受用户归属过滤——传空串 = 全库
	doc, err := s.store.GetDocument(ctx, docID, "")
	if err != nil {
		return err
	}
	if doc == nil {
		return ErrDocumentNotFound
	}

	// 上传时 Accept 只收 .pdf，磁盘文件名固定为 {ID}.pdf（与 Rechunk 同款约定）
	diskPath := filepath.Join(s.uploadDir, doc.ID+".pdf")
	f, err := os.Open(diskPath)
	if err != nil {
		s.fail(doc, fmt.Sprintf("原始 PDF 文件不存在（%v）", err))
		return fmt.Errorf("原始 PDF 文件不存在: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("读取文件信息失败: %w", err)
	}

	l := logger.With("doc_id", doc.ID, "file", doc.Filename)

	// 分块参数：Accept 时已写进文档记录，直接沿用（老数据 0 回退全局默认）
	cfg := chunker.ChunkConfig{Size: doc.ChunkSize, Overlap: doc.ChunkOverlap}
	if cfg.Size <= 0 {
		cfg = s.chunkCfg
	}

	start := time.Now()
	l.Info("ingest.start", "size_bytes", doc.SizeBytes, "chunk_size", cfg.Size, "chunk_overlap", cfg.Overlap)

	// 幂等清场：清掉上次崩溃可能留下的半成品 chunks（正常情况是空删 no-op）
	if err := s.store.DeleteChunks(ctx, doc.ID); err != nil {
		l.Error("store.delete_chunks_failed", "err", err)
		return fmt.Errorf("清理旧分块失败: %w", err)
	}

	// 解析 → 分块 → 公式OCR → 向量化 → 组装（与 Rechunk 共用同一流水线）
	models, err := s.buildChunkModels(ctx, f, fi.Size(), diskPath, doc.Filename, cfg, doc, l)
	if err != nil {
		s.fail(doc, err.Error())
		return err
	}

	// 入库：Accept 阶段已存 pending 记录（chunks 外键依赖已满足），写 chunks 后更新为 ready
	if err := s.store.AddChunks(ctx, models); err != nil {
		l.Error("store.add_chunks_failed", "chunks", len(models), "err", err)
		s.fail(doc, fmt.Sprintf("入库失败: %v", err))
		return err
	}
	doc.ChunkCount = len(models)
	doc.Status = "ready"
	doc.Error = ""
	if err := s.store.SaveDocument(ctx, doc); err != nil {
		return err
	}
	l.Info("ingest.done", "chunks", doc.ChunkCount, "pages", doc.PageCount, "duration_ms", time.Since(start).Milliseconds())
	return nil
}

// buildChunkModels 执行核心流水线：解析 → 分块 → 公式OCR → 向量化 → 组装带向量的分块模型。
// diskPath 是已落盘的 PDF 路径（公式截图 OCR 需要它渲染页面）。
// 失败时返回带上下文的错误、不改动 doc 的状态（由调用方决定如何标记）。
func (s *IngestService) buildChunkModels(ctx context.Context, file io.ReaderAt, size int64, diskPath, filename string, cfg chunker.ChunkConfig, doc *model.Document, l *slog.Logger) ([]model.Chunk, error) {
	// 2. 解析 PDF
	parseStart := time.Now()
	parsed, err := parser.ParsePDF(file, size)
	if err != nil {
		l.Error("parse.failed", "err", err)
		return nil, fmt.Errorf("PDF 解析失败: %w", err)
	}
	doc.Title = parsed.Title
	doc.PageCount = parsed.PageCount
	if doc.Title == "" {
		doc.Title = strings.TrimSuffix(filename, filepath.Ext(filename))
	}
	l.Info("parse.done", "pages", parsed.PageCount, "blocks", countBlocks(parsed.Blocks), "title", doc.Title, "duration_ms", time.Since(parseStart).Milliseconds())

	// 3. 分块：文本块按字符切，表格块按"表头 + N 行"自包含切，公式块整块独立
	chunkStart := time.Now()
	chunks := chunker.SplitBlocks(parsed.Blocks, cfg)
	if len(chunks) == 0 {
		return nil, errors.New("PDF 未能提取到文本内容")
	}
	textChunks, tableChunks, formulaChunks := countChunkTypes(chunks)
	l.Info("chunk.done", "chunks", len(chunks), "text", textChunks, "table", tableChunks, "formula", formulaChunks, "duration_ms", time.Since(chunkStart).Milliseconds())

	// 3.5 公式块截图 OCR：检测出的公式区域 → 渲染 PDF 成图 → 裁剪 → 视觉模型还原 LaTeX。
	// 成功替换 content（LaTeX 文本直接向量化），失败降级为原扁平文本（不阻塞文档摄入）。
	// 未配置 render.python_cmd（未装 pymupdf）时跳过截图，同样退回扁平文本。
	if s.renderCfg.PythonCmd != "" {
		total := 0
		for i := range chunks {
			if chunks[i].Formula {
				total++
			}
		}
		done := 0
		ocrOK := 0
		l.Info("formula_ocr.start", "count", total, "model", s.ai.FormulaModel())
		for i := range chunks {
			if !chunks[i].Formula {
				continue
			}
			done++
			ocrStart := time.Now()
			img, err := s.renderCfg.CropPage(diskPath, chunks[i].Page, render.Region{
				Left:  chunks[i].Left,
				Right: chunks[i].Right,
				Top:   chunks[i].Top,
				Bot:   chunks[i].Bot,
			})
			if err != nil {
				l.Debug("formula_ocr.render_failed", "seq", done, "total", total, "page", chunks[i].Page, "err", err)
				continue
			}
			latex, err := s.ai.FormulaOCR(ctx, img)
			if err != nil {
				l.Debug("formula_ocr.ocr_failed", "seq", done, "total", total, "page", chunks[i].Page, "duration_ms", time.Since(ocrStart).Milliseconds(), "err", err)
				continue
			}
			if strings.TrimSpace(latex) == "" {
				l.Debug("formula_ocr.empty_result", "seq", done, "total", total, "page", chunks[i].Page)
				continue
			}
			chunks[i].Content = latex
			ocrOK++
			l.Debug("formula_ocr.ok", "seq", done, "total", total, "page", chunks[i].Page, "duration_ms", time.Since(ocrStart).Milliseconds(), "latex", truncate(latex, 80))
		}
		l.Info("formula_ocr.done", "count", total, "ok", ocrOK, "fallback", total-ocrOK)
	}

	// 4. 向量化 + 组装
	contents := make([]string, len(chunks))
	for i, c := range chunks {
		contents[i] = c.Content
	}
	embStart := time.Now()
	l.Debug("embed.start", "chunks", len(chunks), "model", s.ai.EmbedModel(), "batch_size", s.ai.EmbedBatchSize())
	vectors, err := s.ai.Embed(ctx, contents)
	if err != nil {
		l.Error("embed.failed", "chunks", len(chunks), "err", err)
		return nil, fmt.Errorf("向量化失败: %w", err)
	}
	l.Info("embed.done", "chunks", len(chunks), "vectors", len(vectors), "duration_ms", time.Since(embStart).Milliseconds())

	models := make([]model.Chunk, 0, len(chunks))
	for i, c := range chunks {
		models = append(models, model.Chunk{
			ID:         newID(),
			DocumentID: doc.ID,
			Filename:   filename,
			Page:       c.Page,
			Index:      i,
			Content:    c.Content,
			Vector:     vectors[i],
			CreatedAt:  time.Now(),
		})
	}
	return models, nil
}

func (s *IngestService) fail(doc *model.Document, msg string) {
	doc.Status = "failed"
	doc.Error = msg
	_ = s.store.SaveDocument(context.Background(), doc)
}

// Get 查询文档详情（校验归属；userID 为 "" = admin，不过滤）。
func (s *IngestService) Get(ctx context.Context, id string, userID string) (*model.Document, error) {
	return s.store.GetDocument(ctx, id, userID)
}

// List 列出文档（userID 为 "" = admin 看全库；否则只列自己的）。
func (s *IngestService) List(ctx context.Context, userID string) ([]model.Document, error) {
	return s.store.ListDocuments(ctx, userID)
}

// Delete 删除文档及其向量（校验归属；userID 为 "" = admin，可删任何文档）。
func (s *IngestService) Delete(ctx context.Context, id string, userID string) error {
	if err := s.store.DeleteDocument(ctx, id, userID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrDocumentNotFound
		}
		return err
	}
	return nil
}

// countBlocks 统计块流里的块总数（含公式/表格/正文）。
func countBlocks(pages [][]parser.Block) int {
	n := 0
	for _, blocks := range pages {
		n += len(blocks)
	}
	return n
}

// countChunkTypes 统计 chunk 按类型分布：公式 / 表格（Markdown 以 | 开头）/ 正文。
func countChunkTypes(chunks []chunker.Chunk) (text, table, formula int) {
	for _, c := range chunks {
		if c.Formula {
			formula++
		} else if strings.HasPrefix(strings.TrimSpace(c.Content), "|") {
			table++
		} else {
			text++
		}
	}
	return
}
