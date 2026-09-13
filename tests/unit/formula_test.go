package unit

import (
	"strings"
	"testing"

	"github.com/ledongthuc/pdf"

	"paper-rag-backend/internal/parser"
)

func TestIsMathFont(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"CambriaMath", true},
		{"ABCDEF+CambriaMath", true}, // 子集前缀应被忽略
		{"MTExtra", true},
		{"EuclidSymbol", true},
		{"EuclidMathOne", true},
		{"Symbol", true},
		{"Helvetica", false},
		{"TimesNewRoman", false},
		{"SimSun", false},
	}
	for _, c := range cases {
		if got := parser.IsMathFont(c.name); got != c.want {
			t.Fatalf("IsMathFont(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// mfont 构造一个数学字体字形。
func mfont(font, s string, x, y float64) pdf.Text {
	return pdf.Text{Font: font, FontSize: 12, X: x, Y: y, S: s}
}

// nfont 构造一个普通字体字形。
func nfont(s string, x, y float64) pdf.Text {
	return pdf.Text{Font: "SimSun", FontSize: 12, X: x, Y: y, S: s}
}

func TestDetectFormulas_Standalone(t *testing.T) {
	content := pdf.Content{Text: []pdf.Text{
		// 独立行公式（y=100）：a=b c，行内 Y 有轻微波动
		mfont("CambriaMath", "a", 100, 100),
		mfont("CambriaMath", "=", 110, 101),
		mfont("CambriaMath", "b", 120, 100),
		mfont("CambriaMath", "c", 135, 100),
	}}
	formulas := parser.DetectFormulas(content)
	if len(formulas) != 1 {
		t.Fatalf("应检测出 1 个公式区域, got %d", len(formulas))
	}
	if !strings.Contains(formulas[0].Text, "a") || !strings.Contains(formulas[0].Text, "b") {
		t.Fatalf("公式文本不对: %q", formulas[0].Text)
	}
}

func TestDetectFormulas_MultiLineStandalone(t *testing.T) {
	content := pdf.Content{Text: []pdf.Text{
		// 上标场景：x 与上标 2、3 同行（Y 差 <= rowTol），应合并为一个公式
		mfont("CambriaMath", "x", 100, 300),
		mfont("CambriaMath", "2", 110, 303),
		mfont("CambriaMath", "3", 120, 303),
	}}
	formulas := parser.DetectFormulas(content)
	if len(formulas) != 1 {
		t.Fatalf("应检测出 1 个公式区域, got %d", len(formulas))
	}
	if !strings.Contains(formulas[0].Text, "x") || !strings.Contains(formulas[0].Text, "2") {
		t.Fatalf("公式文本不对: %q", formulas[0].Text)
	}
}

func TestDetectFormulas_FractionMerge(t *testing.T) {
	content := pdf.Content{Text: []pdf.Text{
		// 分式 c/a = b：分子 c 在 y=100，分母 a 在 y=92，应合并成一个公式
		mfont("CambriaMath", "c", 100, 100),
		mfont("CambriaMath", "a", 100, 92),
		mfont("CambriaMath", "=", 120, 96),
		mfont("CambriaMath", "b", 130, 96),
	}}
	formulas := parser.DetectFormulas(content)
	if len(formulas) != 1 {
		t.Fatalf("分式应合并为 1 个公式区域, got %d", len(formulas))
	}
	// 分子/分母两行应保留 \n 分隔
	if !strings.Contains(formulas[0].Text, "\n") {
		t.Fatalf("分式应保留行分隔: %q", formulas[0].Text)
	}
}

func TestDetectFormulas_InlineRejected(t *testing.T) {
	content := pdf.Content{Text: []pdf.Text{
		// 正文文字："当" 紧挨着内联公式 "x>0" 的左边
		nfont("当", 80, 100),
		mfont("CambriaMath", "x", 90, 100),
		mfont("CambriaMath", ">", 100, 100),
		mfont("CambriaMath", "0", 110, 100),
		nfont("时", 122, 100),
	}}
	formulas := parser.DetectFormulas(content)
	if len(formulas) != 0 {
		t.Fatalf("内联公式不应被识别为独立公式, got %d: %+v", len(formulas), formulas)
	}
}

func TestDetectFormulas_NoMathFont(t *testing.T) {
	content := pdf.Content{Text: []pdf.Text{
		nfont("这", 80, 100),
		nfont("是", 90, 100),
		nfont("正", 100, 100),
		nfont("文", 110, 100),
	}}
	if got := parser.DetectFormulas(content); got != nil {
		t.Fatalf("无数学字体不应检测到公式, got %+v", got)
	}
}
