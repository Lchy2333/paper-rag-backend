package chunker

import (
	"strings"
	"testing"
)

func TestSplitPages_SingleShortPage(t *testing.T) {
	chunks := SplitPages([]string{"hello world"}, ChunkConfig{Size: 800, Overlap: 100})
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
	cfg := ChunkConfig{Size: 60, Overlap: 15}
	chunks := SplitPages([]string{text}, cfg)

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
	chunks := SplitPages([]string{"   \n  "}, ChunkConfig{Size: 100, Overlap: 20})
	if len(chunks) != 0 {
		t.Fatalf("空白页应产生 0 个 chunk, got %d", len(chunks))
	}
}

func TestSplitPages_PageNumbering(t *testing.T) {
	chunks := SplitPages([]string{"page one content", "page two content"}, ChunkConfig{Size: 1000, Overlap: 0})
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	if chunks[0].Page != 1 || chunks[1].Page != 2 {
		t.Fatalf("page numbers = %d,%d, want 1,2", chunks[0].Page, chunks[1].Page)
	}
}
