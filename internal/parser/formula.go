package parser

import (
	"sort"
	"strings"

	"github.com/ledongthuc/pdf"
)

// FormulaRegion 是识别出的一个公式区域。
// v1 只检测"独立行公式"：独占一行（或相邻数行）的公式，正文内联公式不处理。
type FormulaRegion struct {
	Idx         []int   // 属于该公式的字形在 content.Text 中的下标（用于从正文中剔除）
	Top, Bot    float64 // y 范围（PDF 坐标，Y 向上）
	Left, Right float64 // x 范围
	Text        string  // 扁平文本，行间用 \n 分隔（行 = 一个视觉行）
}

// 公式检测阈值（单位 pt）。
const (
	formulaRowTol    = 3.0  // 同一视觉行的 y 波动容差（覆盖上下标轻微偏移）
	formulaMergeGap  = 8.0  // 相邻视觉行合并为一个公式的最大间隙（覆盖分式分子/分母）
	formulaInlineGap = 12.0 // 判定"内联公式"的 x 方向容差（≈1×字号）：正文文字离公式边界
	// 多近算同行。如"当 x>0 时"里 x>0 两侧的中文与公式仅隔 ~10pt；
	// 而独立公式行尾的编号（如"(1)"）通常距公式数百 pt，不会被误判。
)

// IsMathFont 判断字体名是否属于数学公式字体。
// fork 的 Text.Font 已去掉子集前缀（如 "ABCDEF+CambriaMath" → "CambriaMath"，
// 见 page.go 的 BaseFont 处理），所以直接做子串匹配即可。
// 导出供 tests/unit 黑盒测试使用。
func IsMathFont(name string) bool {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "math"): // CambriaMath 等 Word 公式字体
		return true
	case strings.Contains(n, "mtextra"), strings.Contains(n, "mt extra"): // MathType
		return true
	case strings.Contains(n, "euclid"): // MathType Euclid Symbol/Math 系列
		return true
	case strings.Contains(n, "symbol"): // SymbolMT 等老式公式符号字体
		return true
	case strings.Contains(n, "timesnewromanps"): // 老式 Word 公式的 Times New Roman 变量（正/斜体）
		return true
	case strings.Contains(n, "zjsjgb"), strings.Contains(n, "zjsjgc"), strings.Contains(n, "zjsjgu"): // 方正书版 ZJSJ 公式变量/希腊字母字体（gb=变量主体, gc=希腊字母）
		return true
	case strings.Contains(n, "cambria") && strings.Contains(n, "math"): // Cambria Math 变体
		return true
	}
	return false
}

// DetectFormulas 从页面文本中检测公式区域。
//
// 原理：按数学字体挑出字形 → 按 y 聚成视觉行 → 相邻行合并成区域 →
// 过滤掉与正文文字同行（内联公式）的区域，只保留独立行公式。
// 导出供 tests/unit 黑盒测试使用。
func DetectFormulas(content pdf.Content) []FormulaRegion {
	// 1. 收集数学字体字形，同时记录原始下标（用于从正文流剔除）
	var mathGlyphs []idxText
	for i, t := range content.Text {
		if IsMathFont(t.Font) {
			mathGlyphs = append(mathGlyphs, idxText{i, t})
		}
	}
	if len(mathGlyphs) == 0 {
		return nil
	}

	// 合并阈值按公式字号自适应：分式分子/分母、积分上下限在垂直方向
	// 相距可达 1.5×字号，固定小阈值会把同一个公式拆散。页面上不同公式
	// 之间间距通常远大于此（一个完整行高 ~2×字号+），不会误合。
	mergeGap := formulaMergeGap
	{
		var sizes []float64
		for _, g := range mathGlyphs {
			sizes = append(sizes, g.t.FontSize)
		}
		sort.Float64s(sizes)
		med := sizes[len(sizes)/2] // 中位字号
		if gap := 1.5 * med; gap > mergeGap {
			mergeGap = gap
		}
	}

	// 2. 按 (y 降序, x 升序) 排序
	sort.Slice(mathGlyphs, func(i, j int) bool {
		if mathGlyphs[i].t.Y != mathGlyphs[j].t.Y {
			return mathGlyphs[i].t.Y > mathGlyphs[j].t.Y
		}
		return mathGlyphs[i].t.X < mathGlyphs[j].t.X
	})

	// 3. 聚成视觉行：字形 y 与当前行顶部的差 <= rowTol 视为同一行
	var rows []formulaRow
	for _, g := range mathGlyphs {
		if len(rows) > 0 {
			r := &rows[len(rows)-1]
			if abs(g.t.Y-r.maxY) <= formulaRowTol {
				r.glyphs = append(r.glyphs, g)
				if g.t.Y < r.minY {
					r.minY = g.t.Y
				}
				continue
			}
		}
		rows = append(rows, formulaRow{
			glyphs: []idxText{g},
			maxY:   g.t.Y,
			minY:   g.t.Y,
		})
	}

	// 4. 相邻行按 y 间隙合并成区域
	var regions []FormulaRegion
	var cur FormulaRegion
	var curRows []formulaRow
	flush := func() {
		if len(curRows) == 0 {
			return
		}
		if !isInlineFormula(content, curRows) {
			cur.Text = rowsToText(curRows)
			if isRealFormula(cur.Text) {
				regions = append(regions, cur)
			}
		}
		cur = FormulaRegion{}
		curRows = nil
	}
	for _, r := range rows {
		if len(curRows) > 0 {
			prev := curRows[len(curRows)-1]
			if prev.minY-r.maxY > mergeGap {
				flush()
			}
		}
		curRows = append(curRows, r)
		for _, g := range r.glyphs {
			cur.Idx = append(cur.Idx, g.idx)
			if len(cur.Idx) == 1 {
				cur.Top, cur.Bot = g.t.Y, g.t.Y
				cur.Left, cur.Right = g.t.X, g.t.X
			} else {
				if g.t.Y > cur.Top {
					cur.Top = g.t.Y
				}
				if g.t.Y < cur.Bot {
					cur.Bot = g.t.Y
				}
				if g.t.X < cur.Left {
					cur.Left = g.t.X
				}
				if g.t.X > cur.Right {
					cur.Right = g.t.X
				}
			}
		}
	}
	flush()
	return regions
}

// formulaRow 是一个视觉行：同一行内的字形按 x 升序。
type formulaRow struct {
	glyphs     []idxText
	maxY, minY float64
}

// isInlineFormula 判断公式区域是否嵌在正文句子里（内联公式）。//
// 判定：公式区域左侧（x < 公式左边界 - 容差）存在同行紧邻的非数学字形，
// 视为内联公式（如"当 x>0 时"里 x>0 的左边是"当"），v1 不处理。
//
// 只查左侧不查右侧的原因：方正书版排版论文中，独立公式行后面常跟
// 中文变量注释（如"V=0.5πtf'Dcotθ(5)  式中 V 为…"），若查右侧会把
// 这类"独立公式+注释"误判成内联而漏检。
//
// 注意：空白字形（空格/换行）不参与判断——排版产生的空白噪声
// 不应让独立公式被误判为内联。
func isInlineFormula(content pdf.Content, rows []formulaRow) bool {
	for _, r := range rows {
		left := r.glyphs[0].t.X
		for _, g := range r.glyphs {
			if g.t.X < left {
				left = g.t.X
			}
		}
		for _, t := range content.Text {
			if IsMathFont(t.Font) {
				continue
			}
			if strings.TrimSpace(t.S) == "" {
				continue // 空白/换行字形忽略
			}
			if abs(t.Y-r.maxY) > formulaRowTol {
				continue
			}
			// 左侧同行：x 在公式左边界左侧且紧邻（间距 <= 容差）
			if t.X >= left-formulaInlineGap && t.X <= left {
				return true
			}
		}
	}
	return false
}

// isRealFormula 判断检测出的区域是否真是公式，过滤标点/孤字符误报。
// 方正书版等排版的公式可能只有少数几个数学字形，但真正的公式至少含
// 一个字母/数字（变量），纯标点（。、—《》等）是正文字形误入公式字体，
// 应丢弃。
func isRealFormula(text string) bool {
	cleaned := CleanText(text)
	if len([]rune(cleaned)) < 2 {
		return false
	}
	for _, r := range cleaned {
		// 字母、数字、数学符号任一出现即视为真公式
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return true
		}
		switch r {
		case '=', '+', '-', '×', '÷', '±', 'π', 'θ', 'α', 'β', 'γ', 'ρ', 'σ', 'τ', 'φ', 'λ', 'μ', '∑', '∫', '√', '≥', '≤', '∞':
			return true
		}
	}
	return false
}

// rowsToText 把公式区域的行拼成扁平文本：行内按 x 升序，行间用 \n 分隔。
func rowsToText(rows []formulaRow) string {
	var parts []string
	for _, r := range rows {
		var b strings.Builder
		for _, g := range r.glyphs {
			b.WriteString(g.t.S)
		}
		if s := CleanText(b.String()); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}
