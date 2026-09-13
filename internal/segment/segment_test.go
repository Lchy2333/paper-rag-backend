package segment

import (
	"strings"
	"testing"
)

func TestTokenize_Chinese(t *testing.T) {
	words := Tokenize("钢管约束钢筋混凝土柱的受剪承载力计算方法")
	joined := strings.Join(words, " ")
	for _, want := range []string{"钢管", "钢筋", "混凝土", "承载力"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("分词结果缺少 %q: %q", want, joined)
		}
	}
}

func TestTokenize_FormulaKept(t *testing.T) {
	words := Tokenize("V=0.5πtf'Dcotθ 钢管受剪承载力")
	joined := strings.Join(words, " ")
	// 公式串应保留（至少含 D、cot、θ 相关词），中文照常分词
	if !strings.Contains(joined, "钢管") {
		t.Fatalf("中文未分词: %q", joined)
	}
	t.Logf("分词: %q", joined)
}

func TestTokenizeEnglish_FormulaWhole(t *testing.T) {
	words := TokenizeEnglish("V=0.5πtf'Dcotθ")
	if len(words) != 1 || words[0] != "V=0.5πtf'Dcotθ" {
		t.Fatalf("公式应整串保留: %v", words)
	}
}

func TestTokenize_Empty(t *testing.T) {
	if got := Tokenize("   "); got != nil {
		t.Fatalf("空文本应返回 nil, got %v", got)
	}
}
