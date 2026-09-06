package parser

import (
	"sort"
	"strings"

	"github.com/ledongthuc/pdf"
)

// line 是网格线：fix 为固定坐标（H 线为 y、V 线为 x），lo/hi 为线段覆盖范围。
type line struct {
	fix float64
	lo  float64
	hi  float64
}

// table 是识别出的一个框线表格，bot/top 为其 y 范围（PDF 坐标，Y 向上）。
type table struct {
	top float64
	bot float64
	md  string
}

// 表格检测阈值（单位 pt）。
const (
	gridTol    = 1.5 // 同一网格线的坐标聚类容差
	lineMinLen = 3.0 // 小于该长度的矩形视为线帽噪声
)

// clusterLines 按 fix 聚类（tol 内视为同一条网格线），
// 同一 fix 簇内先按 lo 排序再按连续性分段，避免把跨多个表格的
// 断线段合并成一条贯穿长线，也避免同 fix 乱序导致的翻倍。
func clusterLines(ls []line, tol float64) []line {
	sort.Slice(ls, func(i, j int) bool { return ls[i].fix < ls[j].fix })
	var out []line
	i := 0
	for i < len(ls) {
		j := i
		for j < len(ls) && ls[j].fix-ls[i].fix <= tol {
			j++
		}
		group := ls[i:j]
		sort.Slice(group, func(a, b int) bool { return group[a].lo < group[b].lo })
		cur := group[0]
		for k := 1; k < len(group); k++ {
			if group[k].lo <= cur.hi+2 {
				if group[k].lo < cur.lo {
					cur.lo = group[k].lo
				}
				if group[k].hi > cur.hi {
					cur.hi = group[k].hi
				}
			} else {
				out = append(out, cur)
				cur = group[k]
			}
		}
		out = append(out, cur)
		i = j
	}
	return out
}

// detectTables 从页面 Content 检测由矩形（含框线/背景）构成的表格，
// 把每个表格渲染为 Markdown 后返回，按顶部 y 从高到低排序。
// v1 只处理"有完整框线网格"的表格，无框线/纯线段表格不识别。
func detectTables(content pdf.Content) []table {
	var hs, vs []line
	for _, rc := range content.Rect {
		if rc.Max.X-rc.Min.X < lineMinLen || rc.Max.Y-rc.Min.Y < lineMinLen {
			continue
		}
		hs = append(hs, line{rc.Min.Y, rc.Min.X, rc.Max.X}, line{rc.Max.Y, rc.Min.X, rc.Max.X})
		vs = append(vs, line{rc.Min.X, rc.Min.Y, rc.Max.Y}, line{rc.Max.X, rc.Min.Y, rc.Max.Y})
	}
	hl := clusterLines(hs, gridTol)
	vl := clusterLines(vs, gridTol)
	if len(hl) < 2 || len(vl) < 2 {
		return nil
	}

	// 剔除页面边框线：坐标处于极值且跨度贯穿（> 半个最大跨度）。
	var maxHSpan, maxVSpan float64
	for _, h := range hl {
		if h.hi-h.lo > maxHSpan {
			maxHSpan = h.hi - h.lo
		}
	}
	for _, v := range vl {
		if v.hi-v.lo > maxVSpan {
			maxVSpan = v.hi - v.lo
		}
	}
	minHFix, maxHFix := hl[0].fix, hl[0].fix
	for _, h := range hl {
		if h.fix < minHFix {
			minHFix = h.fix
		}
		if h.fix > maxHFix {
			maxHFix = h.fix
		}
	}
	minVFix, maxVFix := vl[0].fix, vl[0].fix
	for _, v := range vl {
		if v.fix < minVFix {
			minVFix = v.fix
		}
		if v.fix > maxVFix {
			maxVFix = v.fix
		}
	}
	hl2 := hl[:0]
	for _, h := range hl {
		if (h.fix == minHFix || h.fix == maxHFix) && h.hi-h.lo > 0.5*maxHSpan {
			continue
		}
		hl2 = append(hl2, h)
	}
	hl = hl2
	vl2 := vl[:0]
	for _, v := range vl {
		if (v.fix == minVFix || v.fix == maxVFix) && v.hi-v.lo > 0.5*maxVSpan {
			continue
		}
		vl2 = append(vl2, v)
	}
	vl = vl2
	if len(hl) < 2 || len(vl) < 2 {
		return nil
	}

	// 表格分离：同一表格的各条 V 线段 span（y 范围）几乎一致，
	// 因此按 (lo, hi) 聚类 V 线段即可得到每个表格的列线与垂直范围，
	// 不受各表格行高差异影响（行高 15~32pt 与表间空隙重叠时仍稳定）。
	sort.Slice(vl, func(i, j int) bool {
		if vl[i].lo != vl[j].lo {
			return vl[i].lo < vl[j].lo
		}
		return vl[i].hi < vl[j].hi
	})
	const spanTol = 5.0
	var vgroups [][]line
	for _, v := range vl {
		if len(vgroups) > 0 {
			g := vgroups[len(vgroups)-1]
			if abs(v.lo-g[0].lo) <= spanTol && abs(v.hi-g[0].hi) <= spanTol {
				vgroups[len(vgroups)-1] = append(vgroups[len(vgroups)-1], v)
				continue
			}
		}
		vgroups = append(vgroups, []line{v})
	}

	var tables []table
	for _, g := range vgroups {
		if len(g) < 2 {
			continue
		}
		bot, top := g[0].lo, g[0].hi
		var xs []float64
		for _, v := range g {
			xs = append(xs, v.fix)
		}
		sort.Float64s(xs)
		xs = dedupeFloat(xs)
		if len(xs) < 2 {
			continue
		}
		var gh []line
		for _, h := range hl {
			if h.fix >= bot-2 && h.fix <= top+2 {
				gh = append(gh, h)
			}
		}
		if len(gh) < 2 {
			continue
		}
		var gtxt []idxText
		for i, t := range content.Text {
			if t.Y > top+5 || t.Y < bot-5 {
				continue
			}
			gtxt = append(gtxt, idxText{i, t})
		}
		md := renderTable(gh, g, gtxt)
		if strings.TrimSpace(md) == "" {
			continue
		}
		tables = append(tables, table{top: top, bot: bot, md: md})
	}
	sort.Slice(tables, func(i, j int) bool { return tables[i].top > tables[j].top })
	return tables
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func dedupeFloat(xs []float64) []float64 {
	out := xs[:0]
	for i, x := range xs {
		if i == 0 || x-out[len(out)-1] > 0.5 {
			out = append(out, x)
		}
	}
	return out
}

type idxText struct {
	idx int
	t   pdf.Text
}

// renderTable 用 H/V 网格线 + 页面文本构建 Markdown 表格。
func renderTable(hs, vs []line, texts []idxText) string {
	var hys, vxs []float64
	for _, h := range hs {
		hys = append(hys, h.fix)
	}
	for _, v := range vs {
		vxs = append(vxs, v.fix)
	}
	sort.Float64s(hys)
	sort.Float64s(vxs)
	nr, nc := len(hys)-1, len(vxs)-1
	if nr < 1 || nc < 1 {
		return ""
	}
	tableTop, tableBot := hys[len(hys)-1], hys[0]
	cells := make([][]idxText, nr*nc)
	for _, it := range texts {
		if cleanText(it.t.S) == "" {
			continue
		}
		if it.t.Y < tableBot-1 || it.t.Y > tableTop+1 {
			continue
		}
		r := -1
		for i := 0; i < nr; i++ {
			if it.t.Y >= hys[i]-0.5 && it.t.Y < hys[i+1]+0.5 {
				r = i
				break
			}
		}
		c := -1
		for j := 0; j < nc; j++ {
			if it.t.X >= vxs[j]-0.5 && it.t.X < vxs[j+1]+0.5 {
				c = j
				break
			}
		}
		if r >= 0 && c >= 0 {
			cells[r*nc+c] = append(cells[r*nc+c], it)
		}
	}
	var sb strings.Builder
	for r := nr - 1; r >= 0; r-- { // 从上到下
		var rowCells []string
		for c := 0; c < nc; c++ {
			txts := cells[r*nc+c]
			// 按库输出的原始下标排序：部分字体（如 DengXian 子集）
			// 宽度表缺失，导致字符 X 坐标不递增，按 X 排序会乱序。
			sort.Slice(txts, func(i, j int) bool { return txts[i].idx < txts[j].idx })
			var cell strings.Builder
			for _, it := range txts {
				cell.WriteString(it.t.S)
			}
			cellStr := cleanText(cell.String())
			cellStr = strings.ReplaceAll(cellStr, "|", "\\|")
			rowCells = append(rowCells, cellStr)
		}
		allEmpty := true
		for _, c := range rowCells {
			if c != "" {
				allEmpty = false
				break
			}
		}
		if allEmpty {
			continue
		}
		for c := 0; c < nc; c++ {
			if c > 0 {
				sb.WriteString(" | ")
			}
			sb.WriteString(rowCells[c])
		}
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// cleanText 过滤解码失败字符与空白。
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, "")
	var sb strings.Builder
	for _, r := range s {
		if r == 0xFFFD || r == '\n' || r == '\r' || r == 0 {
			continue
		}
		sb.WriteRune(r)
	}
	return strings.TrimSpace(sb.String())
}
