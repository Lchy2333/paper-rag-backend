package unit

import (
	"strings"
	"testing"

	"paper-rag-backend/internal/chunker"
	"paper-rag-backend/internal/parser"
)

func TestSplitPages_SingleShortPage(t *testing.T) {
	chunks := chunker.SplitPages([]string{"hello world"}, chunker.ChunkConfig{Size: 800, Overlap: 100})
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}
	if chunks[0].Content != "hello world" {
		t.Fatalf("content = %q", chunks[0].Content)
	}
	if chunks[0].Page != 1 {
		t.Fatalf("page = %d, want 1", chunks[0].Page)
	}
}

func TestSplitPages_MultiChunkOverlap(t *testing.T) {
	text := strings.Repeat("这是一段用于测试分块的中文内容。", 50)
	cfg := chunker.ChunkConfig{Size: 60, Overlap: 15}
	chunks := chunker.SplitPages([]string{text}, cfg)

	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want >= 2", len(chunks))
	}
	// 检查相邻 chunk 有重叠（首个 chunk 长度不超过 size + overlap）
	if len([]rune(chunks[0].Content)) > cfg.Size+cfg.Overlap {
		t.Fatalf("首个 chunk 过长: %d", len([]rune(chunks[0].Content)))
	}
	// 全部内容应被覆盖
	joined := strings.Join(func() []string {
		ss := make([]string, len(chunks))
		for i, c := range chunks {
			ss[i] = c.Content
		}
		return ss
	}(), "")
	for _, r := range []rune("这是一段用于测试分块的中文内容。") {
		if !strings.ContainsRune(joined, r) {
			t.Fatalf("内容丢失: %q", string(r))
		}
	}
}

func TestSplitPages_BlankPage(t *testing.T) {
	chunks := chunker.SplitPages([]string{"   \n  "}, chunker.ChunkConfig{Size: 100, Overlap: 20})
	if len(chunks) != 0 {
		t.Fatalf("空白页应产生 0 个 chunk, got %d", len(chunks))
	}
}

func TestSplitPages_PageNumbering(t *testing.T) {
	chunks := chunker.SplitPages([]string{"page one content", "page two content"}, chunker.ChunkConfig{Size: 1000, Overlap: 0})
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	if chunks[0].Page != 1 || chunks[1].Page != 2 {
		t.Fatalf("page numbers = %d,%d, want 1,2", chunks[0].Page, chunks[1].Page)
	}
}

// TestSplitBlocks_TableSelfContained 验证大表格按"表头 + N 行"切成
// 多个子块，且每个子块都重复携带表头（自包含）。
func TestSplitBlocks_TableSelfContained(t *testing.T) {
	header := "名称 | 数量"
	var sb strings.Builder
	sb.WriteString(header)
	for i := 0; i < 20; i++ {
		sb.WriteString("\n苹果 | ")
		sb.WriteByte(byte('A' + i))
	}
	blocks := [][]parser.Block{
		{{Kind: parser.BlockTable, Content: sb.String()}},
	}
	chunks := chunker.SplitBlocks(blocks, chunker.ChunkConfig{Size: 60, Overlap: 0})
	if len(chunks) < 2 {
		t.Fatalf("大表格应切成多个子块, got %d", len(chunks))
	}
	for i, c := range chunks {
		if !strings.HasPrefix(c.Content, header+"\n") {
			t.Fatalf("子块 %d 应以表头开头（自包含）: %q", i, c.Content)
		}
	}
	// 整表数据应无遗漏（除表头重复外）
	joined := ""
	for _, c := range chunks {
		joined += c.Content
	}
	for i := 0; i < 20; i++ {
		if !strings.Contains(joined, "苹果 | "+string(rune('A'+i))) {
			t.Fatalf("数据行 %d 丢失", i)
		}
	}
}

// TestSplitBlocks_MixedTextAndTable 验证文本块照常按字符切、表格块独立成 chunk，
// 且块顺序保持版面顺序。
func TestSplitBlocks_MixedTextAndTable(t *testing.T) {
	blocks := [][]parser.Block{
		{
			{Kind: parser.BlockText, Content: "上面是一段说明文字。"},
			{Kind: parser.BlockTable, Content: "名称 | 数量\n苹果 | 3"},
			{Kind: parser.BlockText, Content: "下面是另一段文字。"},
		},
	}
	chunks := chunker.SplitBlocks(blocks, chunker.ChunkConfig{Size: 800, Overlap: 0})
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3 (text+table+text)", len(chunks))
	}
	if !strings.Contains(chunks[0].Content, "说明文字") {
		t.Fatalf("第 1 块应为前文: %q", chunks[0].Content)
	}
	if !strings.HasPrefix(chunks[1].Content, "名称 | 数量") {
		t.Fatalf("第 2 块应为表格: %q", chunks[1].Content)
	}
	if !strings.Contains(chunks[2].Content, "另一段文字") {
		t.Fatalf("第 3 块应为后文: %q", chunks[2].Content)
	}
}
