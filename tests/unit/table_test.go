package unit

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"paper-rag-backend/internal/parser"
)

// buildTablePDF 生成一个 content stream 可自定义的单页 PDF。
// 通过 re 操作符绘制矩形（表格框线/单元格），Tj 绘制文本，
// 与现有 buildTestPDF 使用同一套 PDF 骨架。
func buildTablePDF(content string) ([]byte, error) {
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

// parsePage 把 content stream 解析成第一页文本。
func parsePage(content string) (string, error) {
	data, err := buildTablePDF(content)
	if err != nil {
		return "", err
	}
	doc, err := parser.ParsePDF(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	if doc.PageCount < 1 {
		return "", fmt.Errorf("PageCount = %d, want >= 1", doc.PageCount)
	}
	return doc.Pages[0], nil
}

// frameRect 是占满页面的背景矩形，让"页边框剔除"逻辑把它的边线当作页面边框，
// 从而不影响页面中间的表格线。
const frameRect = "0 0 612 792 re S"

// TestParsePDF_TableToMarkdown 验证含框线表格的 PDF 被解析为 Markdown 表格。
func TestParsePDF_TableToMarkdown(t *testing.T) {
	content := strings.Join([]string{
		frameRect,
		// 表头行（y 70~90）：2 个单元格
		"50 70 100 20 re S",
		"150 70 100 20 re S",
		// 数据行（y 50~70）：2 个单元格
		"50 50 100 20 re S",
		"150 50 100 20 re S",
		// 文本：表头在顶部行带（Y=80），数据在底部行带（Y=60）
		"BT /F1 12 Tf 60 80 Td (Name) Tj ET",
		"BT /F1 12 Tf 160 80 Td (Qty) Tj ET",
		"BT /F1 12 Tf 60 60 Td (Apple) Tj ET",
		"BT /F1 12 Tf 160 60 Td (3) Tj ET",
	}, "\n")

	out, err := parsePage(content)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !strings.Contains(out, "Name | Qty") {
		t.Fatalf("输出缺少表头行, got:\n%s", out)
	}
	if !strings.Contains(out, "Apple | 3") {
		t.Fatalf("输出缺少数据行, got:\n%s", out)
	}
}

// TestParsePDF_MultiTable 验证同页两个表格被正确分离并保持版面顺序。
func TestParsePDF_MultiTable(t *testing.T) {
	content := strings.Join([]string{
		frameRect,
		// 表格 A（y 200~260）
		"50 200 100 30 re S", "150 200 100 30 re S",
		"50 230 100 30 re S", "150 230 100 30 re S",
		// 表格 B（y 300~360）
		"50 300 100 30 re S", "150 300 100 30 re S",
		"50 330 100 30 re S", "150 330 100 30 re S",
		"BT /F1 12 Tf 60 245 Td (A1) Tj ET",
		"BT /F1 12 Tf 160 245 Td (A2) Tj ET",
		"BT /F1 12 Tf 60 345 Td (B1) Tj ET",
		"BT /F1 12 Tf 160 345 Td (B2) Tj ET",
	}, "\n")

	out, err := parsePage(content)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 顶部表格（B）应排在底部表格（A）之前
	bi, ai := strings.Index(out, "B1 | B2"), strings.Index(out, "A1 | A2")
	if bi < 0 || ai < 0 {
		t.Fatalf("输出缺少表格内容, got:\n%s", out)
	}
	if bi > ai {
		t.Fatalf("版面顺序错误：顶部表格 B 应在前, got:\n%s", out)
	}
}

// TestParsePDF_BlocksContainTable 验证 ParsePDF 输出的块流包含独立的表格块。
func TestParsePDF_BlocksContainTable(t *testing.T) {
	content := strings.Join([]string{
		frameRect,
		"50 70 100 20 re S", "150 70 100 20 re S",
		"50 50 100 20 re S", "150 50 100 20 re S",
		"BT /F1 12 Tf 60 80 Td (Name) Tj ET",
		"BT /F1 12 Tf 160 80 Td (Qty) Tj ET",
		"BT /F1 12 Tf 60 60 Td (Apple) Tj ET",
		"BT /F1 12 Tf 160 60 Td (3) Tj ET",
	}, "\n")

	data, err := buildTablePDF(content)
	if err != nil {
		t.Fatalf("生成测试 PDF 失败: %v", err)
	}
	doc, err := parser.ParsePDF(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("ParsePDF 失败: %v", err)
	}
	if len(doc.Blocks) != 1 {
		t.Fatalf("Blocks 页数 = %d, want 1", len(doc.Blocks))
	}
	foundTable := false
	for _, b := range doc.Blocks[0] {
		if b.Kind == parser.BlockTable && strings.Contains(b.Content, "Name | Qty") {
			foundTable = true
		}
	}
	if !foundTable {
		t.Fatalf("块流中缺少表格块, blocks: %+v", doc.Blocks[0])
	}
}
func TestParsePDF_NoTableNotDetected(t *testing.T) {
	content := strings.Join([]string{
		frameRect,
		"BT /F1 12 Tf 60 100 Td (This is plain text, not a table.) Tj ET",
	}, "\n")

	out, err := parsePage(content)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if strings.Contains(out, "|") {
		t.Fatalf("纯文本不应被识别为表格, got:\n%s", out)
	}
	if !strings.Contains(out, "plain text") {
		t.Fatalf("纯文本内容丢失, got:\n%s", out)
	}
}
