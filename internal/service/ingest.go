package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"paper-rag-backend/internal/ai"
	"paper-rag-backend/internal/chunker"
	"paper-rag-backend/internal/config"
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

// Process 处理一个上传的文件并返回入库后的文档。
func (s *IngestService) Process(ctx context.Context, file io.ReaderAt, size int64, filename string) (*model.Document, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext != ".pdf" {
		return nil, ErrUnsupportedType
	}

	if err := os.MkdirAll(s.uploadDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建上传目录失败: %w", err)
	}

	doc := &model.Document{
		ID:        newID(),
		UserID:    defaultOwnerUserID,
		Filename:  filename,
		Status:    "pending",
		SizeBytes: size,
		CreatedAt: time.Now(),
	}

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
		return existing, ErrDuplicateDocument
	}

	// 2. 解析 PDF
	parsed, err := parser.ParsePDF(file, size)
	if err != nil {
		s.fail(doc, fmt.Sprintf("PDF 解析失败: %v", err))
		return doc, err
	}
	doc.Title = parsed.Title
	doc.PageCount = parsed.PageCount
	if doc.Title == "" {
		doc.Title = strings.TrimSuffix(filename, ext)
	}

	// 3. 分块：文本块按字符切，表格块按"表头 + N 行"自包含切，公式块整块独立
	chunks := chunker.SplitBlocks(parsed.Blocks, s.chunkCfg)
	if len(chunks) == 0 {
		s.fail(doc, "PDF 未能提取到文本内容")
		return doc, errors.New("PDF 未能提取到文本内容")
	}

	// 3.5 公式块截图 OCR：检测出的公式区域 → 渲染 PDF 成图 → 裁剪 → 视觉模型还原 LaTeX。
	// 成功替换 content（LaTeX 文本直接向量化），失败降级为原扁平文本（不阻塞文档摄入）。
	// 未配置 render.python_cmd（未装 pymupdf）时跳过截图，同样退回扁平文本。
	if s.renderCfg.PythonCmd != "" {
		diskPath := filepath.Join(s.uploadDir, doc.ID+ext)
		total := 0
		for i := range chunks {
			if chunks[i].Formula {
				total++
			}
		}
		done := 0
		log.Printf("[公式OCR] 文档 %s 共 %d 个公式块，模型=%s", filename, total, s.ai.FormulaModel())
		for i := range chunks {
			if !chunks[i].Formula {
				continue
			}
			done++
			start := time.Now()
			img, err := s.renderCfg.CropPage(diskPath, chunks[i].Page, render.Region{
				Left:  chunks[i].Left,
				Right: chunks[i].Right,
				Top:   chunks[i].Top,
				Bot:   chunks[i].Bot,
			})
			if err != nil {
				log.Printf("[公式OCR] (%d/%d) p%d 渲染失败(%v)，降级为扁平文本", done, total, chunks[i].Page, err)
				continue
			}
			latex, err := s.ai.FormulaOCR(ctx, img)
			if err != nil {
				log.Printf("[公式OCR] (%d/%d) p%d OCR 失败(%v) 耗时%v，降级为扁平文本",
					done, total, chunks[i].Page, err, time.Since(start))
				continue
			}
			if strings.TrimSpace(latex) == "" {
				log.Printf("[公式OCR] (%d/%d) p%d OCR 返回空，降级为扁平文本", done, total, chunks[i].Page)
				continue
			}
			chunks[i].Content = latex
			log.Printf("[公式OCR] (%d/%d) p%d OK 耗时%v → %s", done, total, chunks[i].Page, time.Since(start), truncate(latex, 80))
		}
	}

	// 4. 向量化 + 组装
	contents := make([]string, len(chunks))
	for i, c := range chunks {
		contents[i] = c.Content
	}
	embStart := time.Now()
	vectors, err := s.ai.Embed(ctx, contents)
	if err != nil {
		s.fail(doc, fmt.Sprintf("向量化失败: %v", err))
		return doc, err
	}
	log.Printf("[摄入] 向量化完成：%d 个 chunk，耗时%v", len(chunks), time.Since(embStart))

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

	// 5. 入库：先存文档（chunks 外键依赖它），再存分块，最后更新文档为 ready
	if err := s.store.SaveDocument(ctx, doc); err != nil {
		return doc, err
	}
	if err := s.store.AddChunks(ctx, models); err != nil {
		s.fail(doc, fmt.Sprintf("入库失败: %v", err))
		return doc, err
	}
	doc.ChunkCount = len(models)
	doc.Status = "ready"
	if err := s.store.SaveDocument(ctx, doc); err != nil {
		return doc, err
	}
	return doc, nil
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
