package parser

import (
	"bytes"
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
	end, err := trimTrailingJunk(r, size)
	if err != nil {
		return nil, fmt.Errorf("无法读取 PDF: %w", err)
	}
	reader, err := pdf.NewReader(io.NewSectionReader(r, 0, end), end)
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

// trimTrailingJunk 返回 PDF 有效部分的结束位置。
// 部分来源（如知网 CNKI）会在 %%EOF 之后追加 XML 元数据，而解析库
// ledongthuc/pdf 要求文件末尾正好是 %%EOF（只读最后 100 字节做 HasSuffix 校验），
// 否则报 missing %%EOF。这里从尾部往前找最后一个 %%EOF，若其后还有内容则
// 截断到该位置；找不到 %%EOF 则原样返回 size（交给解析库报正常错误）。
func trimTrailingJunk(r io.ReaderAt, size int64) (int64, error) {
	const scanWindow = 1 << 20 // 最多往回扫描 1MB，覆盖常规追加的元数据
	start := size - scanWindow
	if start < 0 {
		start = 0
	}
	buf := make([]byte, size-start)
	n, err := r.ReadAt(buf, start)
	if err != nil && err != io.EOF {
		return size, err
	}
	buf = buf[:n]

	idx := bytes.LastIndex(buf, []byte("%%EOF"))
	if idx < 0 {
		return size, nil
	}
	end := start + int64(idx) + int64(len("%%EOF"))
	if end >= size {
		return size, nil
	}
	return end, nil
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
