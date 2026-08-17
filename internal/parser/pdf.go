package parser

import (
	"fmt"
	"io"
	"strings"

	"github.com/ledongthuc/pdf"
)

// ParsedDocument 是 PDF 解析后的结果，按页保留文本。
type ParsedDocument struct {
	Title     string
	PageCount int
	Pages     []string // Pages[i] 为第 i 页的文本
}

// ParsePDF 从 reader 解析 PDF，返回按页划分的文本。
func ParsePDF(r io.ReaderAt, size int64) (*ParsedDocument, error) {
	reader, err := pdf.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("无法读取 PDF: %w", err)
	}

	pageCount := reader.NumPage()
	doc := &ParsedDocument{
		Title:     inferTitle(reader),
		PageCount: pageCount,
		Pages:     make([]string, 0, pageCount),
	}

	for i := 1; i <= pageCount; i++ {
		p := reader.Page(i)
		text, err := p.GetPlainText(nil)
		if err != nil {
			return nil, fmt.Errorf("解析第 %d 页失败: %w", i, err)
		}
		doc.Pages = append(doc.Pages, normalizeText(text))
	}
	return doc, nil
}

// inferTitle 从元数据或第一页前几行猜测文档标题。
func inferTitle(r *pdf.Reader) string {
	if t := r.Trailer().Key("Root").Key("Info").Key("Title"); !t.IsNull() {
		if s := t.Text(); strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	if r.NumPage() >= 1 {
		text, err := r.Page(1).GetPlainText(nil)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(normalizeText(text)), "\n")
			for _, l := range lines {
				l = strings.TrimSpace(l)
				if len(l) > 5 && len(l) <= 200 {
					return l
				}
			}
		}
	}
	return ""
}

// normalizeText 清洗解析出来的原始文本：合并行尾换行、去重多余空白。
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\u00a0", " ")
	var sb strings.Builder
	prevBlank := false
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if !prevBlank {
				sb.WriteByte('\n')
			}
			prevBlank = true
			continue
		}
		prevBlank = false
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return strings.TrimSpace(sb.String())
}
