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

// TestSplitBlocks_MixedTextAndTable 验证同页文本块跨表格拼接后统一滑窗切，
// 表格块独立成 chunk（挂起到文本之后）。
func TestSplitBlocks_MixedTextAndTable(t *testing.T) {
	blocks := [][]parser.Block{
		{
			{Kind: parser.BlockText, Content: "上面是一段说明文字。"},
			{Kind: parser.BlockTable, Content: "名称 | 数量\n苹果 | 3"},
			{Kind: parser.BlockText, Content: "下面是另一段文字。"},
		},
	}
	chunks := chunker.SplitBlocks(blocks, chunker.ChunkConfig{Size: 800, Overlap: 0})
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2 (merged text + table)", len(chunks))
	}
	if !strings.Contains(chunks[0].Content, "说明文字") || !strings.Contains(chunks[0].Content, "另一段文字") {
		t.Fatalf("表格前后的文本应合并为一个 chunk: %q", chunks[0].Content)
	}
	if !strings.HasPrefix(chunks[1].Content, "名称 | 数量") {
		t.Fatalf("第 2 块应为表格: %q", chunks[1].Content)
	}
}

// TestSplitBlocks_MergesAdjacentTextSlivers 验证相邻文本块先拼接再切：
// 假公式/表格把正文切成一行一块的碎片时，合并后不再产出碎 chunk；
// 公式块仍独立成 chunk。
func TestSplitBlocks_MergesAdjacentTextSlivers(t *testing.T) {
	blocks := [][]parser.Block{
		{
			{Kind: parser.BlockText, Content: "人工智能信息技术"},
			{Kind: parser.BlockFormula, Content: "1 674 16 33"},
			{Kind: parser.BlockText, Content: "物联网智慧建筑"},
			{Kind: parser.BlockFormula, Content: "2 117 17 32"},
			{Kind: parser.BlockText, Content: "云计算决策系统"},
		},
	}
	chunks := chunker.SplitBlocks(blocks, chunker.ChunkConfig{Size: 800, Overlap: 0})
	// 5 个碎块 → 3 个 chunk：三段文本合并为 1 个 + 2 个公式块
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3 (merged text + 2 formulas), got %+v", len(chunks), chunks)
	}
	if !strings.Contains(chunks[0].Content, "人工智能信息技术") ||
		!strings.Contains(chunks[0].Content, "物联网智慧建筑") ||
		!strings.Contains(chunks[0].Content, "云计算决策系统") {
		t.Fatalf("相邻文本块应合并为一个 chunk: %q", chunks[0].Content)
	}
	if chunks[1].Content != "1 674 16 33" || !chunks[1].Formula {
		t.Fatalf("公式块应保持独立: %+v", chunks[1])
	}
}

// TestSplitBlocks_PseudoTableFallsBackToText 伪表格防御回归：
// detectTables 整页误判产出的"表格"每行只有一格（无 " | " 列分隔），
// 不得把首行正文当表头重复携带到每个子块开头（实测 74 字正文重复 3 次），
// 应退化为普通文本切分。
func TestSplitBlocks_PseudoTableFallsBackToText(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("对于CTRC-200试件来说，两者承载力基本接近，选取承载力相对较高的CTRC-200-b短柱试件进行对比分析。")
	for i := 0; i < 15; i++ {
		sb.WriteString("\n图3轴标签碎片数据行")
	}
	blocks := [][]parser.Block{
		{{Kind: parser.BlockTable, Content: sb.String()}},
	}
	chunks := chunker.SplitBlocks(blocks, chunker.ChunkConfig{Size: 120, Overlap: 0})
	if len(chunks) < 2 {
		t.Fatalf("伪表格应退化为文本切分, got %d", len(chunks))
	}
	for i, c := range chunks {
		if strings.HasPrefix(c.Content, "对于CTRC-200试件来说") && i > 0 {
			t.Fatalf("子块 %d 不应重复携带首行正文（表头）: %q", i, c.Content)
		}
	}
}
