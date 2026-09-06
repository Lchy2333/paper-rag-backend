package unit

import (
	"testing"

	"paper-rag-backend/internal/parser"
)

// TestClusterLines_合并与分段 验证同 fix 线合并、跨表格断线段分段。
// 注意：TestDetectTables_* 白盒用例未迁移——它们与同目录 table_test.go
// 的黑盒用例（TestParsePDF_TableToMarkdown / TestParsePDF_MultiTable /
// TestParsePDF_NoTableNotDetected）完全重复，黑盒版本更能代表真实解析路径。
func TestClusterLines_合并与分段(t *testing.T) {
	// 同 Fix=10 的三条重叠线应合并为一条
	merged := parser.ClusterLines([]parser.Line{
		{Fix: 10, Lo: 0, Hi: 50},
		{Fix: 10, Lo: 20, Hi: 80},
		{Fix: 10.4, Lo: 70, Hi: 100},
	}, 1.5)
	if len(merged) != 1 {
		t.Fatalf("重叠线应合并为一条, got %d", len(merged))
	}
	if merged[0].Hi != 100 || merged[0].Lo != 0 {
		t.Fatalf("合并后范围错误: %+v", merged[0])
	}
	// 同 Fix 但中间断开（两个表格共用列线）应分成两条
	split := parser.ClusterLines([]parser.Line{
		{Fix: 10, Lo: 0, Hi: 20},
		{Fix: 10, Lo: 100, Hi: 130},
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
		if got := parser.CleanText(in); got != want {
			t.Errorf("CleanText(%q) = %q, want %q", in, got, want)
		}
	}
}
