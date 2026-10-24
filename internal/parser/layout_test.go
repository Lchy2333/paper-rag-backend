package parser

import (
	"strings"
	"testing"

	"github.com/ledongthuc/pdf"
)

// mkLine 生成一行字形：每个字符一个 pdf.Text，宽度固定为 w。
func mkLine(y, x0 float64, s string, w float64) []pdf.Text {
	out := make([]pdf.Text, 0, len([]rune(s)))
	for i, r := range []rune(s) {
		out = append(out, pdf.Text{
			Font:     "Test",
			FontSize: w,
			X:        x0 + float64(i)*w,
			Y:        y,
			W:        w,
			S:        string(r),
		})
	}
	return out
}

// 双栏页：左栏 63-286、右栏 307-530，通栏标题跨两栏。
// 左栏 5 行，最后一行为短行（段落结束）；右栏 5 行。
func doubleColumnPage() []pdf.Text {
	var texts []pdf.Text
	texts = append(texts, mkLine(780, 63, strings.Repeat("题", 48), 9.7)...) // 63..528，通栏
	for i := 0; i < 5; i++ {
		y := 760 - float64(i)*14
		n := 23
		if i == 4 {
			n = 12 // 段落最后一行没排满
		}
		texts = append(texts, mkLine(y, 63, strings.Repeat("左", n), 9.7)...)   // 63..286
		texts = append(texts, mkLine(y, 307, strings.Repeat("右", 23), 9.7)...) // 307..530
	}
	return texts
}

func TestLayoutPage_双栏先左后右(t *testing.T) {
	texts := doubleColumnPage()
	lines := layoutPage(texts)
	if len(lines) != 11 {
		t.Fatalf("期望 11 行（1 通栏 + 5 左 + 5 右），got %d", len(lines))
	}
	got := make([]string, len(lines))
	for i, ln := range lines {
		got[i] = lineString(texts, ln)
	}
	want := []string{strings.Repeat("题", 48)}
	for i := 0; i < 4; i++ {
		want = append(want, strings.Repeat("左", 23))
	}
	want = append(want, strings.Repeat("左", 12))
	for i := 0; i < 5; i++ {
		want = append(want, strings.Repeat("右", 23))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 行顺序错误: got %q", i, got[i])
		}
	}
	if !lines[1].breakBefore {
		t.Fatalf("通栏标题之后应换行: %+v", lines[1])
	}
	if !lines[6].breakBefore {
		t.Fatalf("左栏末行未排满时，右栏首行应换行: %+v", lines[6])
	}
}

func TestLayoutPage_同一行浮点噪声(t *testing.T) {
	texts := mkLine(759.173150, 63, "对钢管约束效", 9.99)
	texts = append(texts, mkLine(759.173141, 63+7*9.99, "土柱的轴压力学性能", 9.99)...)
	lines := layoutPage(texts)
	if len(lines) != 1 {
		t.Fatalf("9e-6 的 Y 差应视为同一行，got %d 行", len(lines))
	}
	if s := lineString(texts, lines[0]); s != "对钢管约束效土柱的轴压力学性能" {
		t.Fatalf("行内顺序错误: %q", s)
	}
}

func TestLayoutPage_标点基线偏移(t *testing.T) {
	// 逗号比正文高 0.485pt（同属视觉行），且原点落在前一字的格子里 → 应与前一字交换
	texts := mkLine(759.173150, 63, "对钢管约束效应进行分析", 9.99)
	texts = append(texts, pdf.Text{X: 73.0, Y: 759.658286, W: 9.99, FontSize: 9.99, S: "，"})
	lines := layoutPage(texts)
	if len(lines) != 1 {
		t.Fatalf("0.485pt 基线偏移不应拆行，got %d 行", len(lines))
	}
	got := lineString(texts, lines[0])
	if !strings.HasPrefix(got, "对，钢") {
		t.Fatalf("标点应留在行内并归位: %q", got)
	}
}

func TestLayoutPage_单栏软换行合并(t *testing.T) {
	texts := mkLine(760, 72, strings.Repeat("甲", 33), 9.7) // 72..392，排满
	texts = append(texts, mkLine(746, 72, strings.Repeat("乙", 33), 9.7)...)
	if got := layoutText(texts); strings.Contains(got, "\n") {
		t.Fatalf("排满的软换行不应保留换行: %q", got)
	}
}

func TestLayoutPage_段落缩进保留换行(t *testing.T) {
	texts := mkLine(760, 72, strings.Repeat("甲", 33), 9.7)                  // 排满
	texts = append(texts, mkLine(746, 91, strings.Repeat("乙", 30), 9.7)...) // 首行缩进 19pt
	if got := layoutText(texts); !strings.Contains(got, "\n乙") {
		t.Fatalf("段首缩进应保留换行: %q", got)
	}
}

func TestLayoutPage_跨栏软换行(t *testing.T) {
	var texts []pdf.Text
	for i := 0; i < 5; i++ {
		y := 760 - float64(i)*14
		texts = append(texts, mkLine(y, 63, strings.Repeat("左", 23), 9.7)...)
		texts = append(texts, mkLine(y, 307, strings.Repeat("右", 23), 9.7)...)
	}
	// 左栏每行都排满 + 右栏首行不缩进 → 跨栏续接，不换行
	lines := layoutPage(texts)
	if len(lines) != 10 {
		t.Fatalf("期望 10 行, got %d", len(lines))
	}
	if lines[5].breakBefore {
		t.Fatalf("左栏末行排满时，右栏首行应视为续接: %+v", lines[5])
	}
	if got := layoutText(texts); !strings.Contains(got, "左右") {
		t.Fatalf("跨栏处应直接拼接: %q", got)
	}
}

func TestLayoutPage_英文断词与空格(t *testing.T) {
	texts := mkLine(760, 72, strings.Repeat("a", 65)+"-", 5) // 72..397，排满且以连字符结尾
	texts = append(texts, mkLine(746, 72, "bcdef", 5)...)
	if got := layoutText(texts); strings.Contains(got, "-") {
		t.Fatalf("行末断词连字符应删除: %q", got)
	}

	texts = mkLine(760, 72, strings.Repeat("a", 65), 5)
	texts = append(texts, mkLine(746, 72, "bcdef", 5)...)
	if got := layoutText(texts); !strings.Contains(got, "a bcdef") {
		t.Fatalf("拉丁单词跨行应补空格: %q", got)
	}
}

// mkWordLine 生成一行由多个「词」组成的字形：词内字符紧贴（gap=0），词间空出 gap。
// 模拟知网排版：不写空格字形，单词边界只靠几何间隙表达。
func mkWordLine(y, x0, w, gap float64, words ...string) []pdf.Text {
	var out []pdf.Text
	x := x0
	for wi, word := range words {
		if wi > 0 {
			x += gap
		}
		for _, r := range []rune(word) {
			out = append(out, pdf.Text{Font: "Test", FontSize: w, X: x, Y: y, W: w, S: string(r)})
			x += w
		}
	}
	return out
}

func TestLayoutPage_英文词间补空格(t *testing.T) {
	// 词内紧贴（0 < 阈值 0.15×4.4），词间 3.2pt（≈0.36×字号）→ 补空格
	texts := mkWordLine(760, 63, 4.4, 3.2, "Comparison", "of", "failure", "modes")
	lines := layoutPage(texts)
	if len(lines) != 1 {
		t.Fatalf("期望 1 行, got %d", len(lines))
	}
	if got := lineString(texts, lines[0]); got != "Comparison of failure modes" {
		t.Fatalf("词间应补空格: %q", got)
	}
}

func TestLayoutPage_CJK大间隙不补空格(t *testing.T) {
	// 中文排版没有词间空格，大间隙也不应插入
	texts := mkWordLine(760, 63, 9.7, 3.2, "钢管", "混凝土")
	lines := layoutPage(texts)
	if got := lineString(texts, lines[0]); got != "钢管混凝土" {
		t.Fatalf("中文不应补空格: %q", got)
	}
}

func TestLayoutPage_已有空格字形不双写(t *testing.T) {
	// 正常 PDF 存在真实空格字形：空格非拉丁字符，前后相邻对都不触发补写
	texts := mkWordLine(760, 63, 4.4, 3.2, "abc", "def")
	spaceGlyph := pdf.Text{Font: "Test", FontSize: 4.4, X: 63 + 3*4.4 + 1.0, Y: 760, W: 1.2, S: " "}
	texts = append(texts[:3], append([]pdf.Text{spaceGlyph}, texts[3:]...)...)
	lines := layoutPage(texts)
	if got := lineString(texts, lines[0]); got != "abc def" {
		t.Fatalf("已有空格字形时不应双写: %q", got)
	}
}

// narrowBookPage 模拟小开本扫描书（GB 规范式）的双栏页：
// 版心窄（pageW≈415）、栏缝宽（45pt，超过旧固定上限 36pt）、
// 右栏段首缩进使各行行首 x 参差（旧「完全一致空档」聚类会失效）。
func narrowBookPage() []pdf.Text {
	var texts []pdf.Text
	for i := 0; i < 8; i++ {
		y := 760 - float64(i)*13
		texts = append(texts, mkLine(y, 36, strings.Repeat("左", 21), 9)...) // 36..225
		rx := 271.0
		switch i { // 右栏段首缩进两字，行首 x 参差
		case 1:
			rx = 289
		case 4:
			rx = 295
		}
		texts = append(texts, mkLine(y, rx, strings.Repeat("右", 20), 9)...) // 右栏 271..451
	}
	return texts
}

// checkColumnOrder 断言行序为「先左后右」：含"左"的行全部在含"右"的行之前。
func checkColumnOrder(t *testing.T, texts []pdf.Text, lines []layoutLine, leftRows, rightRows int) {
	t.Helper()
	lastLeft, firstRight := -1, len(lines)
	leftCnt, rightCnt := 0, 0
	for i, ln := range lines {
		s := lineString(texts, ln)
		switch {
		case strings.Contains(s, "左"):
			lastLeft = i
			leftCnt++
		case strings.Contains(s, "右"):
			if firstRight == len(lines) {
				firstRight = i
			}
			rightCnt++
		}
	}
	if leftCnt != leftRows || rightCnt != rightRows {
		t.Fatalf("左右栏行数错误: left=%d (want %d), right=%d (want %d)", leftCnt, leftRows, rightCnt, rightRows)
	}
	if lastLeft > firstRight {
		t.Fatalf("应先左栏后右栏: 左栏末行 idx=%d, 右栏首行 idx=%d", lastLeft, firstRight)
	}
}

// TestLayoutPage_小开本大栏缝双栏 修复回归：栏缝 45pt 超过旧固定上限 36pt、
// 右栏行首缩进参差破坏「完全一致空档」聚类——GB 规范双栏漏检的两个病灶。
func TestLayoutPage_小开本大栏缝双栏(t *testing.T) {
	texts := narrowBookPage()
	lines := layoutPage(texts)
	checkColumnOrder(t, texts, lines, 8, 8)
}

// TestLayoutPage_空格尾巴不撑大版心 修复回归：页眉空白行的空格字形把版心
// 撑大近一倍后，[0.3W,0.7W] 搜索带右移错过真实栏缝（GB 规范实测 pageW 440→760）。
func TestLayoutPage_空格尾巴不撑大版心(t *testing.T) {
	texts := narrowBookPage()
	for i := 0; i < 26; i++ { // 页眉空白行：空格字形一路排到 x≈782
		texts = append(texts, pdf.Text{Font: "Test", FontSize: 7, X: 600 + float64(i)*7, Y: 800, W: 7, S: " "})
	}
	lines := layoutPage(texts)
	checkColumnOrder(t, texts, lines, 8, 8)
}

// TestLayoutPage_表格列间隙不判双栏 修复回归：通栏大表格的列间隙（132pt）
// 反复出现且两侧都有内容，但远超自适应上限（0.15×版心宽），不得判成栏缝。
func TestLayoutPage_表格列间隙不判双栏(t *testing.T) {
	var texts []pdf.Text
	for i := 0; i < 10; i++ {
		y := 760 - float64(i)*14
		texts = append(texts, mkLine(y, 63, strings.Repeat("甲", 19), 9.8)...)  // 63..249
		texts = append(texts, mkLine(y, 381, strings.Repeat("乙", 15), 9.8)...) // 381..528
	}
	lines := layoutPage(texts)
	if len(lines) != 10 {
		t.Fatalf("宽空档应被拒判（单栏 10 行）, got %d", len(lines))
	}
	for i, ln := range lines {
		if ln.col != 0 {
			t.Fatalf("第 %d 行不应被切到右栏: %+v", i, ln)
		}
	}
}

// TestLayoutPage_零宽字形剔除 修复回归：图轴标签渲染层的零宽/零字号字形
// （实测 x=8, w=0, fs=0 的 "y/n/o/l"）以独立行或行首字形混入正文
// （"beamlnolocation"），应在入口剔除且行内下标仍指向原始入参。
func TestLayoutPage_零宽字形剔除(t *testing.T) {
	texts := mkLine(760, 63, "beamlocation", 9)
	// 独立零宽行 + 行首零宽字形
	texts = append(texts, pdf.Text{Font: "Times-Roman", X: 8, Y: 764, W: 0, FontSize: 0, S: "n"})
	texts = append(texts, pdf.Text{Font: "Times-Roman", X: 8, Y: 760, W: 0, FontSize: 0, S: "o"})
	texts = append(texts[:3], append([]pdf.Text{{Font: "Times-Roman", X: 8, Y: 760, W: 0, FontSize: 0, S: "y"}}, texts[3:]...)...)
	lines := layoutPage(texts)
	if len(lines) != 1 {
		t.Fatalf("期望 1 行, got %d", len(lines))
	}
	if got := lineString(texts, lines[0]); got != "beamlocation" {
		t.Fatalf("零宽字形应被剔除: %q", got)
	}
}

// TestLayoutPage_孤立上标行丢弃 修复回归：悬浮于两行正文之间的孤立小字行
// （实测 fs=6.97 单字形 "0"，正文 9.96）输出后插在词间（"strength is 0
// proportional"），应整行丢弃；同字号的段落短行（「规定。」）不受影响。
func TestLayoutPage_孤立上标行丢弃(t *testing.T) {
	var texts []pdf.Text
	texts = append(texts, mkLine(760, 63, "theultimatestrengthis", 9.96)...)
	// 孤立上标行：1 字形、小字号、Y 介于两行之间
	texts = append(texts, pdf.Text{Font: "Test", X: 261, Y: 756.5, W: 1.88, FontSize: 6.97, S: "0"})
	texts = append(texts, mkLine(753, 63, "proportionaltothecompressive", 9.96)...)
	// 段落短行：与正文同字号，必须保留
	texts = append(texts, mkLine(739, 63, "规定。", 9.96)...)
	lines := layoutPage(texts)
	var got []string
	for _, ln := range lines {
		got = append(got, lineString(texts, ln))
	}
	want := []string{"theultimatestrengthis", "proportionaltothecompressive", "规定。"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("孤立上标行应被丢弃且短行保留: got %v", got)
	}
}
