package unit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"paper-rag-backend/internal/parser"
)

var wsRe = regexp.MustCompile(`\s+`)

// stripWS 去除所有空白，用于对"事实"和"解析文本"做逐字精确匹配。
func stripWS(s string) string {
	return wsRe.ReplaceAllString(s, "")
}

// TestParseRealPDFs 解析 goldentest/pdf/ 下的真实 PDF（黄金数据集论文），
// 回归验证 ledongthuc/pdf 的 T* 假字符问题已修复。
// 这些 PDF 由 temp.py 生成、内容已知；goldentest 未入库时自动跳过，
// 与集成测试"没有密码就跳过"的约定保持一致。
func TestParseRealPDFs(t *testing.T) {
	pdfs, err := filepath.Glob("../../goldentest/pdf/*.pdf")
	if err != nil {
		t.Fatalf("glob goldentest: %v", err)
	}
	if len(pdfs) == 0 {
		t.Skip("goldentest/pdf/*.pdf 不存在，跳过真实 PDF 解析用例")
	}

	// 修复前 GetPlainText 在 T* 换行处把字节 0x0A 解码成假字符的典型残留。
	spurious := []string{
		"连续端观测到",   // 论文一：混入"端"
		"木质素磺磺酸盐",  // 论文二：混入"磺"
		"25页6GB",     // 论文二：混入"页"
		"如页何",       // 论文二：混入"页"
		"感知境如何",    // 论文三：混入"境"
		"类别稀的召回率",  // 论文五：混入"稀"
		"概书率高于",    // 论文四：混入"书"
	}

	for _, f := range pdfs {
		f := f
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("读取 PDF 失败: %v", err)
			}
			doc, err := parser.ParsePDF(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				t.Fatalf("ParsePDF 失败: %v", err)
			}
			if doc.PageCount < 1 {
				t.Fatalf("PageCount = %d, want >= 1", doc.PageCount)
			}
			if doc.Title == "" {
				t.Errorf("未能推断文档标题")
			}
			all := strings.Join(doc.Pages, "\n")
			for i, p := range doc.Pages {
				if strings.TrimSpace(p) == "" {
					t.Errorf("第 %d 页为空", i+1)
				}
			}
			for _, s := range spurious {
				if strings.Contains(all, s) {
					t.Errorf("发现 T* 假字符残留 %q", s)
				}
			}
		})
	}
}

// TestParseRealPDFs_FactsMatch 用 goldentest/facts.json 验证 50 条事实
// 在解析出的文本中都能逐字精确匹配（忽略空白后）。
// 这是黄金数据集的核心验收标准：修复前只有 26/50 能匹配。
func TestParseRealPDFs_FactsMatch(t *testing.T) {
	jsonData, err := os.ReadFile("../../goldentest/facts.json")
	if err != nil {
		t.Skip("goldentest/facts.json 不存在，跳过事实匹配用例")
	}
	pdfs, err := filepath.Glob("../../goldentest/pdf/*.pdf")
	if err != nil {
		t.Fatalf("glob goldentest: %v", err)
	}
	if len(pdfs) == 0 {
		t.Skip("goldentest/pdf/*.pdf 不存在，跳过事实匹配用例")
	}

	var facts struct {
		Atomic      map[string]string `json:"atomic"`
		Distractors map[string]string `json:"distractors"`
	}
	if err := json.Unmarshal(jsonData, &facts); err != nil {
		t.Fatalf("解析 facts.json 失败: %v", err)
	}

	var all strings.Builder
	for _, f := range pdfs {
		d, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", f, err)
		}
		doc, err := parser.ParsePDF(bytes.NewReader(d), int64(len(d)))
		if err != nil {
			t.Fatalf("ParsePDF(%s) 失败: %v", f, err)
		}
		all.WriteString(strings.Join(doc.Pages, "\n"))
	}
	corpus := stripWS(all.String())

	var miss []string
	for fid, fact := range facts.Atomic {
		if !strings.Contains(corpus, stripWS(fact)) {
			miss = append(miss, fid)
		}
	}
	// 干扰项按设计"不在"语料中（防幻觉测试项）：出现在语料里反而说明设计错误。
	for fid, fact := range facts.Distractors {
		if strings.Contains(corpus, stripWS(fact)) {
			miss = append(miss, fid+"（干扰项不应在语料中）")
		}
	}
	if len(miss) > 0 {
		t.Errorf("有 %d 条事实匹配异常: %v", len(miss), miss)
	}
}
