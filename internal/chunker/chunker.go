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

// SplitBlocks 按页切分块流：同页文本块统一拼接后按字符滑窗切，表格块按
// "表头 + N 行"自包含地切成若干子块（每个子块都带表头），公式块整块独立。
//
// 为什么文本块要跨表格/公式拼接：parser 慢路径里每插入一个表格/公式块就封口一次
// 文本，上游检测抖动会把正文切成一行一块的碎片（实测汉斯 PDF 第 7 页 64 块）；
// 即使公式全为真，公式密集页同样产生大量碎文本块。chunker 只切不合的话，
// 块粒度直接变成 chunk 粒度，产出大量 ≤10 字的碎 chunk。
//
// 拼接用 \n\n 连接，splitText 的断点查找优先在 \n 处断——两段真正不相关的大段
// 拼接后超过 Size 时几乎必然从接缝断开，退化为原边界；只有小碎片被吸收。
// 表格/公式块挂起到同页文本之后输出：chunk 顺序不影响检索（按相似度返回），
// 仅影响展示顺序。
func SplitBlocks(pages [][]parser.Block, cfg ChunkConfig) []Chunk {
	if cfg.Size <= 0 {
		cfg.Size = 800
	}
	if cfg.Overlap >= cfg.Size {
		cfg.Overlap = cfg.Size / 4
	}

	var chunks []Chunk
	for pi, blocks := range pages {
		var texts []string
		var deferred []Chunk
		for _, b := range blocks {
			switch b.Kind {
			case parser.BlockTable:
				deferred = append(deferred, splitTable(b.Content, cfg, pi+1)...)
			case parser.BlockFormula:
				// 公式块自包含，整块成一个 chunk，不与其他文本混切
				if s := strings.TrimSpace(b.Content); s != "" {
					deferred = append(deferred, Chunk{
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
				texts = append(texts, b.Content)
			}
		}
		chunks = append(chunks, splitText(strings.Join(texts, "\n\n"), cfg, pi+1)...)
		chunks = append(chunks, deferred...)
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
//
// 伪表格防御：真表格由 renderTable 输出，多列行的单元格以 " | " 连接。上游
// detectTables 把整页误判成表格时（图坐标系框线+表格框线聚组、列线检测失败），
// 产出的"表格"每行只有一格、没有 " | " 分隔——此时把首行正文当表头重复携带，
// 会让同一句话在多个 chunk 开头重复（实测汉斯 PDF 第 4 页 74 字正文重复 3 次）。
// 判据：整块含 " | " 的行数 < 2，判定无列结构，退化为普通文本切分。
func splitTable(md string, cfg ChunkConfig, page int) []Chunk {
	if strings.Count(md, " | ") < 2 {
		return splitText(md, cfg, page)
	}
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
