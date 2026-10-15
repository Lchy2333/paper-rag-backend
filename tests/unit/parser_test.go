package unit

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"paper-rag-backend/internal/parser"
)

// buildTestPDF 生成一个含指定文本的合法单页 PDF，用于测试解析。
func buildTestPDF(pageText string) ([]byte, error) {
	content := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", escapePDFText(pageText))
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}

	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n", len(objs)+1)
	b.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes(), nil
}

func escapePDFText(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "(", "\\(")
	s = strings.ReplaceAll(s, ")", "\\)")
	return s
}

func TestParsePDF(t *testing.T) {
	expected := "This is a test paper about RAG systems."
	data, err := buildTestPDF(expected)
	if err != nil {
		t.Fatalf("生成测试 PDF 失败: %v", err)
	}

	doc, err := parser.ParsePDF(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("ParsePDF 失败: %v", err)
	}
	if doc.PageCount != 1 {
		t.Fatalf("PageCount = %d, want 1", doc.PageCount)
	}
	if !strings.Contains(doc.Pages[0], "RAG systems") {
		t.Fatalf("页面文本缺少预期内容, got: %q", doc.Pages[0])
	}
}

// TestParsePDF_FastPathNormalized 回归：无表格/无公式的快速路径，块内容必须过
// normalizeText（剥 \x00、\r→\n、折叠空白）。
// 曾踩坑：快速路径漏了 normalizeText，扫描件 OCR 垃圾层的 NUL 进 chunk →
// PostgreSQL text 拒绝 0x00（SQLSTATE 22021），GB 50010-2010 全篇入不了库。
// 断言用 \r 做探针：fork 对 base-14 字体把 NUL 映射成 U+FFFD（不是 \x00），但 \r
// 会原样透传——若 normalizeText 未被调用，块内容会残留 \r（快路径回归即触发）。
func TestParsePDF_FastPathNormalized(t *testing.T) {
	data, err := buildTestPDF("A\x00B\rC")
	if err != nil {
		t.Fatalf("生成测试 PDF 失败: %v", err)
	}

	doc, err := parser.ParsePDF(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("ParsePDF 失败: %v", err)
	}

	// 无表格无公式 → 每页一个 BlockText 块（走 pageText 快速路径）
	if len(doc.Blocks) != 1 || len(doc.Blocks[0]) != 1 || doc.Blocks[0][0].Kind != parser.BlockText {
		t.Fatalf("应走快速路径（单文本块）: %+v", doc.Blocks)
	}
	content := doc.Blocks[0][0].Content
	if strings.Contains(content, "\x00") {
		t.Fatalf("块内容含 NUL 字节（PG 22021 元凶）: %q", content)
	}
	if strings.Contains(content, "\r") {
		t.Fatalf("块内容含 \\r（说明未过 normalizeText）: %q", content)
	}
}

// TestParsePDF_TrailingJunk 模拟知网（CNKI）论文：%%EOF 之后追加 XML 元数据，
// 解析库会报 missing %%EOF，parser 应在解析前容错截断。
func TestParsePDF_TrailingJunk(t *testing.T) {
	expected := "CNKI trailing junk test"
	data, err := buildTestPDF(expected)
	if err != nil {
		t.Fatalf("生成测试 PDF 失败: %v", err)
	}
	junk := []byte("WebFastLoad<FileProperty><Doi /><FileName>1018829180.nh</FileName></FileProperty>")
	data = append(data, junk...)

	doc, err := parser.ParsePDF(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("ParsePDF 遇到尾部附加内容应能解析: %v", err)
	}
	if doc.PageCount != 1 {
		t.Fatalf("PageCount = %d, want 1", doc.PageCount)
	}
	if !strings.Contains(doc.Pages[0], expected) {
		t.Fatalf("页面文本缺少预期内容, got: %q", doc.Pages[0])
	}
}
