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

// defaultOwnerUserID 是当前单用户模式的固定文档归属者。
// 将来接入多用户后，应由登录态/鉴权中间件动态赋值。
const defaultOwnerUserID = "local"

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
// opts 为 nil 或字段为空时回退到文档当前使用的参数，再回退全局默认。
// 失败时不删除已有 chunk（保持旧数据可用），仅返回错误。
func (s *IngestService) Rechunk(ctx context.Context, id string, opts *model.IngestOptions) (*model.Document, error) {
	doc, err := s.store.GetDocument(ctx, id)
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

// Process 处理一个上传的文件并返回入库后的文档。
// opts 可携带文档级分块参数（chunk_size/chunk_overlap），nil 字段回退到 config.yaml 的 rag 默认。
func (s *IngestService) Process(ctx context.Context, file io.ReaderAt, size int64, filename string, opts *model.IngestOptions) (*model.Document, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext != ".pdf" {
		return nil, ErrUnsupportedType
	}

	if err := os.MkdirAll(s.uploadDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建上传目录失败: %w", err)
	}

	// 解析文档级分块参数（缺省用全局默认）；校验失败直接拒绝
	cfg, err := resolveChunkConfig(s.chunkCfg, opts)
	if err != nil {
		return nil, err
	}

	doc := &model.Document{
		ID:           newID(),
		UserID:       defaultOwnerUserID,
		Filename:     filename,
		Status:       "pending",
		SizeBytes:    size,
		ChunkSize:    cfg.Size,
		ChunkOverlap: cfg.Overlap,
		CreatedAt:    time.Now(),
	}

	// 任务级 logger：本文档所有阶段日志都带 doc_id，方便跨阶段/将来异步对账
	l := logger.With("doc_id", doc.ID, "file", filename)
	l.Info("ingest.start", "size_bytes", size, "ext", ext, "chunk_size", cfg.Size, "chunk_overlap", cfg.Overlap)
	start := time.Now()

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
	// 允许重新上传重试。
	existing, err := s.store.FindByContentHash(ctx, doc.ContentHash)
	if err != nil {
		return nil, fmt.Errorf("检查重复文档失败: %w", err)
	}
	if existing != nil && existing.Status != "failed" {
		_ = os.Remove(diskPath)
		l.Info("ingest.duplicate", "existing_doc_id", existing.ID)
		return existing, ErrDuplicateDocument
	}

	// 2-4. 解析 → 分块 → 公式OCR → 向量化 → 组装（与 Rechunk 共用同一流水线）
	models, err := s.buildChunkModels(ctx, file, size, diskPath, filename, cfg, doc, l)
	if err != nil {
		s.fail(doc, err.Error())
		return doc, err
	}

	// 5. 入库：先存文档（chunks 外键依赖它），再存分块，最后更新文档为 ready
	if err := s.store.SaveDocument(ctx, doc); err != nil {
		l.Error("store.save_document_failed", "status", doc.Status, "err", err)
		return doc, err
	}
	if err := s.store.AddChunks(ctx, models); err != nil {
		l.Error("store.add_chunks_failed", "chunks", len(models), "err", err)
		s.fail(doc, fmt.Sprintf("入库失败: %v", err))
		return doc, err
	}
	doc.ChunkCount = len(models)
	doc.Status = "ready"
	if err := s.store.SaveDocument(ctx, doc); err != nil {
		return doc, err
	}
	l.Info("ingest.done", "chunks", doc.ChunkCount, "pages", doc.PageCount, "duration_ms", time.Since(start).Milliseconds())
	return doc, nil
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

// Get 查询文档详情。
func (s *IngestService) Get(ctx context.Context, id string) (*model.Document, error) {
	return s.store.GetDocument(ctx, id)
}

// List 列出所有文档。
func (s *IngestService) List(ctx context.Context) ([]model.Document, error) {
	return s.store.ListDocuments(ctx)
}

// Delete 删除文档及其向量。
func (s *IngestService) Delete(ctx context.Context, id string) error {
	return s.store.DeleteDocument(ctx, id)
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
