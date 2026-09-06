package parser

import (
	"testing"

	"github.com/ledongthuc/pdf"
)

func rect(x0, y0, x1, y1 float64) pdf.Rect {
	return pdf.Rect{Min: pdf.Point{X: x0, Y: y0}, Max: pdf.Point{X: x1, Y: y1}}
}

// pageFrame 构造一个模拟 A4 页面的背景矩形，用于页边框剔除。
func pageFrame(w, h float64) pdf.Rect {
	return rect(0, 0, w, h)
}

// TestClusterLines_合并与分段 验证同 fix 线合并、跨表格断线段分段。
func TestClusterLines_合并与分段(t *testing.T) {
	// 同 fix=10 的三条重叠线应合并为一条
	merged := clusterLines([]line{
		{fix: 10, lo: 0, hi: 50},
		{fix: 10, lo: 20, hi: 80},
		{fix: 10.4, lo: 70, hi: 100},
	}, 1.5)
	if len(merged) != 1 {
		t.Fatalf("重叠线应合并为一条, got %d", len(merged))
	}
	if merged[0].hi != 100 || merged[0].lo != 0 {
		t.Fatalf("合并后范围错误: %+v", merged[0])
	}
	// 同 fix 但中间断开（两个表格共用列线）应分成两条
	split := clusterLines([]line{
		{fix: 10, lo: 0, hi: 20},
		{fix: 10, lo: 100, hi: 130},
	}, 1.5)
	if len(split) != 2 {
		t.Fatalf("断开的线应分成两条, got %d", len(split))
	}
}

// TestCleanText 验证非法字符清理。
func TestCleanText(t *testing.T) {
	cases := map[string]string{
		"12,345.67":        "12,345.67",
		"abc\uFFFDdef":      "abcdef",
		"  abc  \n  def  ": "abc    def",
		"abc\x00def":        "abcdef",
	}
	for in, want := range cases {
		if got := cleanText(in); got != want {
			t.Errorf("cleanText(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDetectTables_基本表格 验证 2 行 3 列表格识别为 Markdown。
func TestDetectTables_基本表格(t *testing.T) {
	content := pdf.Content{
		Rect: []pdf.Rect{
			pageFrame(595, 842),
			rect(50, 70, 150, 90), rect(150, 70, 250, 90), rect(250, 70, 350, 90),
			rect(50, 50, 150, 70), rect(150, 50, 250, 70), rect(250, 50, 350, 70),
		},
		Text: []pdf.Text{
			{X: 60, Y: 80, S: "名"}, {X: 70, Y: 80, S: "称"},
			{X: 160, Y: 80, S: "数"}, {X: 170, Y: 80, S: "量"},
			{X: 260, Y: 80, S: "价"}, {X: 270, Y: 80, S: "格"},
			{X: 60, Y: 60, S: "苹"}, {X: 70, Y: 60, S: "果"},
			{X: 160, Y: 60, S: "3"},
			{X: 260, Y: 60, S: "5"}, {X: 270, Y: 60, S: "."}, {X: 280, Y: 60, S: "5"},
		},
	}
	tables := detectTables(content)
	if len(tables) != 1 {
		t.Fatalf("应识别出 1 个表格, got %d", len(tables))
	}
	want := "名称 | 数量 | 价格\n苹果 | 3 | 5.5"
	if got := tables[0].md; got != want {
		t.Fatalf("Markdown 错误:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestDetectTables_多表格堆叠 验证同页两个表格被正确分离并按版面顺序返回。
func TestDetectTables_多表格堆叠(t *testing.T) {
	content := pdf.Content{
		Rect: []pdf.Rect{
			pageFrame(595, 842),
			rect(50, 200, 150, 230), rect(150, 200, 250, 230),
			rect(50, 230, 150, 260), rect(150, 230, 250, 260),
			rect(50, 300, 150, 330), rect(150, 300, 250, 330),
			rect(50, 330, 150, 360), rect(150, 330, 250, 360),
		},
		Text: []pdf.Text{
			{X: 60, Y: 245, S: "A1"}, {X: 160, Y: 245, S: "A2"},
			{X: 60, Y: 215, S: "A3"}, {X: 160, Y: 215, S: "A4"},
			{X: 60, Y: 345, S: "B1"}, {X: 160, Y: 345, S: "B2"},
			{X: 60, Y: 315, S: "B3"}, {X: 160, Y: 315, S: "B4"},
		},
	}
	tables := detectTables(content)
	if len(tables) != 2 {
		t.Fatalf("应识别出 2 个表格, got %d", len(tables))
	}
	if tables[0].top < tables[1].top {
		t.Fatalf("表格应按顶部 y 降序排列: %+v %+v", tables[0], tables[1])
	}
	if got, want := tables[0].md, "B1 | B2\nB3 | B4"; got != want {
		t.Fatalf("顶部表格内容错误:\ngot:\n%s\nwant:\n%s", got, want)
	}
	if got, want := tables[1].md, "A1 | A2\nA3 | A4"; got != want {
		t.Fatalf("底部表格内容错误:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestDetectTables_无框线不识别 验证无网格线的文本不会被误判为表格。
func TestDetectTables_无框线不识别(t *testing.T) {
	content := pdf.Content{
		Rect: []pdf.Rect{
			pageFrame(595, 842),
		},
		Text: []pdf.Text{
			{X: 60, Y: 100, S: "这是"}, {X: 80, Y: 100, S: "普通文本"},
		},
	}
	if tables := detectTables(content); len(tables) != 0 {
		t.Fatalf("无框线不应识别为表格, got %d", len(tables))
	}
}
