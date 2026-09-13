package chunker

import (
	"strings"
	"unicode/utf8"

	"paper-rag-backend/internal/parser"
)

// Chunk 是一个切分后的文本片段。
type Chunk struct {
	Page    int
	Content string
	// Formula 标记该 chunk 来自公式块（内容为扁平文本，待还原 LaTeX）。
	// 由服务层决定是否调用公式还原。
	Formula bool
	// 公式块 PDF 坐标（Y 向上），用于渲染截图 OCR。仅 Formula==true 时有意义。
	Left, Right, Top, Bot float64
}

// ChunkConfig 控制切分参数。
type ChunkConfig struct {
	Size    int // 每个 chunk 的目标字符数
	Overlap int // 相邻 chunk 重叠字符数
}

// SplitBlocks 按页切分块流：文本块按字符滑窗切，表格块按"表头 + N 行"
// 自包含地切成若干子块（每个子块都带表头），保证值不脱离列名。
func SplitBlocks(pages [][]parser.Block, cfg ChunkConfig) []Chunk {
	if cfg.Size <= 0 {
		cfg.Size = 800
	}
	if cfg.Overlap >= cfg.Size {
		cfg.Overlap = cfg.Size / 4
	}

	var chunks []Chunk
	for pi, blocks := range pages {
		for _, b := range blocks {
			switch b.Kind {
			case parser.BlockTable:
				chunks = append(chunks, splitTable(b.Content, cfg, pi+1)...)
			case parser.BlockFormula:
				// 公式块自包含，整块成一个 chunk，不与其他文本混切
				if s := strings.TrimSpace(b.Content); s != "" {
					chunks = append(chunks, Chunk{
						Page:    pi + 1,
						Content: s,
						Formula: true,
						Left:    b.Left,
						Right:   b.Right,
						Top:     b.Top,
						Bot:     b.Bot,
					})
				}
			default:
				chunks = append(chunks, splitText(b.Content, cfg, pi+1)...)
			}
		}
	}
	return chunks
}

// SplitPages 按页切分文档。pages[i] 对应第 i+1 页文本。
func SplitPages(pages []string, cfg ChunkConfig) []Chunk {
	if cfg.Size <= 0 {
		cfg.Size = 800
	}
	if cfg.Overlap >= cfg.Size {
		cfg.Overlap = cfg.Size / 4
	}

	var chunks []Chunk
	for i, page := range pages {
		chunks = append(chunks, splitText(page, cfg, i+1)...)
	}
	return chunks
}

// splitTable 把 Markdown 表格按"表头 + 若干行"切分成自包含的子块。
// 首行为表头，每个子块都以表头开头，子块大小以接近 cfg.Size 为准。
func splitTable(md string, cfg ChunkConfig, page int) []Chunk {
	lines := strings.Split(md, "\n")
	if len(lines) == 0 {
		return nil
	}
	header := strings.TrimSpace(lines[0])
	var rows []string
	for _, l := range lines[1:] {
		if s := strings.TrimSpace(l); s != "" {
			rows = append(rows, s)
		}
	}
	if header == "" || len(rows) == 0 {
		return nil
	}

	var chunks []Chunk
	var cur []string
	curLen := utf8.RuneCountInString(header)
	flush := func() {
		if len(cur) == 0 {
			return
		}
		content := header + "\n" + strings.Join(cur, "\n")
		chunks = append(chunks, Chunk{Page: page, Content: content})
		cur = nil
		curLen = utf8.RuneCountInString(header)
	}
	for _, r := range rows {
		if len(cur) > 0 && curLen+utf8.RuneCountInString(r)+1 > cfg.Size {
			flush()
		}
		cur = append(cur, r)
		curLen += utf8.RuneCountInString(r) + 1
	}
	flush()
	return chunks
}

// splitText 将单页文本切分成多个 chunk，尽量在段落/句子边界断开。
func splitText(text string, cfg ChunkConfig, page int) []Chunk {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	var chunks []Chunk
	start := 0
	for start < utf8.RuneCountInString(text) {
		end := start + cfg.Size
		if end >= utf8.RuneCountInString(text) {
			chunks = append(chunks, Chunk{Page: page, Content: runeSlice(text, start, utf8.RuneCountInString(text))})
			break
		}

		// 优先在最近的段落/句子边界断开
		b := findBoundary(text, start, end, cfg.Size/3)
		chunks = append(chunks, Chunk{Page: page, Content: runeSlice(text, start, b)})
		start = b - cfg.Overlap
		if start < 0 {
			start = 0
		}
	}

	// 丢弃只有空白的 chunk
	filtered := chunks[:0]
	for _, c := range chunks {
		if strings.TrimSpace(c.Content) != "" {
			filtered = append(filtered, c)
		}
	}
	return filtered
}

// findBoundary 在 [minBound, end] 范围内查找最靠近 end 的边界位置（按 rune 索引）。
func findBoundary(text string, start, end, minBound int) int {
	runes := []rune(text)
	limit := end - minBound
	if limit < start+1 {
		limit = start + 1
	}
	for i := end; i > limit; i-- {
		ch := runes[i-1]
		// 句子/段落边界
		if ch == '\n' || ch == '。' || ch == '！' || ch == '？' || ch == '.' || ch == '!' || ch == '?' || ch == '；' || ch == ';' {
			return i
		}
	}
	return end
}

func runeSlice(s string, from, to int) string {
	rs := []rune(s)
	if from > to {
		from, to = to, from
	}
	if from > len(rs) {
		from = len(rs)
	}
	if to > len(rs) {
		to = len(rs)
	}
	return string(rs[from:to])
}
