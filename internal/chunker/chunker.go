package chunker

import (
	"strings"
	"unicode/utf8"
)

// Chunk 是一个切分后的文本片段。
type Chunk struct {
	Page    int
	Content string
}

// ChunkConfig 控制切分参数。
type ChunkConfig struct {
	Size    int // 每个 chunk 的目标字符数
	Overlap int // 相邻 chunk 重叠字符数
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
