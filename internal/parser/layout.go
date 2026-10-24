package parser

import (
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// 版面重建参数（单位：pt）。
const (
	layoutLineTol    = 2.0  // 同一视觉行允许的 Y 波动；吸收标点基线偏移(≈0.5pt)与浮点误差(≈1e-5)
	layoutMinGutter  = 8.0  // 分栏通道（gutter）的最小宽度
	layoutMaxGutter  = 36.0 // 分栏通道宽度的下限基准；实际上限随版心自适应（见 detectGutter），远小于表格整块间隙
	layoutMinColRows = 3    // 分栏通道两侧至少各需的行数，避免把表格空隙误判成栏
	layoutIndentEm   = 1.5  // 行首缩进 / 上一行未排满的判定阈值（字号倍数）
	layoutWordGapEm  = 0.15 // 英文词间补空格的间隙阈值（字号倍数）；实测词内≈0、词间≈0.36

	// layoutGutterCapMax：自适应通道上限的封顶值，防御超大页面把表格列间隙放进候选。
	layoutGutterCapMax = 96.0

	// layoutGutterCenterLo/Hi：通道中心必须落在版心的这个比例区间内，排除页边距空档。
	layoutGutterCenterLo = 0.3
	layoutGutterCenterHi = 0.7
)

// layoutLine 是一个视觉行：同一基线上的一段或多段字形。
type layoutLine struct {
	idx []int // 指向入参 []pdf.Text 的下标，行内已按 X 升序
	y   float64
	x0  float64 // 左边界
	x1  float64 // 右边界（含字形宽度）

	// col 是分栏归属：0=左栏，1=右栏，2=通栏（跨 gutter 的行，如标题）。
	// 单栏页面所有行都是 0。
	col int

	breakBefore bool // 与上一行之间应换行（段落/分栏/通栏边界）
	stripHyphen bool // 软换行处的英文断词连字符应删除
	spaceBefore bool // 软换行处两侧都是拉丁字母/数字时应补一个空格
}

// layoutRow 是同一基线上的一组字形（粗聚类结果，尚未按 gutter 切分）。
type layoutRow struct {
	y   float64
	idx []int
}

// layoutPage 按人类阅读顺序重排页面字形，返回带换行语义的视觉行序列。
//
// 旧实现直接按 (Y↓, X↑) 全局排序，两个致命问题：
//  1. 双栏页面把两栏当成一整行交错读取（右栏顶行比左栏标题还"高"）；
//  2. 同一视觉行内的亚像素 Y 差与标点基线偏移会被当成不同行/不同位置。
//
// 这里先用行占用剖面找 gutter 判断单栏/双栏，再按 Y 聚类成视觉行（锚点比较，
// 避免链式漂移），把压住 gutter 的行留作通栏行、其余行在 gutter 处切成左右两段，
// 最后按「通栏切开、栏内先左后右」展平成阅读顺序。
func layoutPage(texts []pdf.Text) []layoutLine {
	if len(texts) == 0 {
		return nil
	}

	// 0) 剔除零宽/零字号垃圾字形，并保持原始下标映射（ln.idx 仍指向入参 texts）。
	// 典型：环梁论文的图轴标签渲染层（x=8, w=0, fs=0 的 Times-Roman "y/n/o/l"），
	// 会以独立单字形行或行首字形混入正文（实测 "beamlnolocation"）。
	filt := make([]pdf.Text, 0, len(texts))
	remap := make([]int, 0, len(texts))
	for i, t := range texts {
		if t.W <= 0.01 && t.FontSize <= 0.01 {
			continue
		}
		filt = append(filt, t)
		remap = append(remap, i)
	}
	if len(filt) == 0 {
		return nil
	}
	texts = filt

	// 1) 版心范围 + 粗聚类成行
	pageX0, pageX1 := glyphBounds(texts)
	pageW := pageX1 - pageX0
	rows := clusterRows(texts)

	// 2) gutter 检测
	gl, gr := 0.0, 0.0
	if pageW > 0 {
		gl, gr = detectGutter(texts, rows, pageX0, pageW)
	}

	// 3) 按 gutter 把行切成栏内行段
	lines := buildLines(texts, rows, gl, gr)

	// 4) 各栏的左右基准：左基准用于判行首缩进，右基准用于判上一行是否排满。
	colX0 := [3]float64{math.Inf(1), math.Inf(1), pageX0}
	colX1 := [3]float64{math.Inf(-1), math.Inf(-1), pageX1}
	for _, ln := range lines {
		c := ln.col
		colX0[c] = math.Min(colX0[c], ln.x0)
		colX1[c] = math.Max(colX1[c], ln.x1)
	}
	for c := 0; c < 2; c++ {
		if math.IsInf(colX0[c], 1) {
			colX0[c] = pageX0
		}
		if math.IsInf(colX1[c], -1) {
			colX1[c] = pageX1
		}
	}
	// 通栏行的边界就是版心
	colX0[2], colX1[2] = pageX0, pageX1

	// 4) 展平为阅读顺序：通栏行切开前后两个 band，band 内先左栏（Y↓）再右栏（Y↓）
	var ordered []layoutLine
	var leftCol, rightCol []layoutLine
	flush := func() {
		ordered = append(ordered, leftCol...)
		ordered = append(ordered, rightCol...)
		leftCol, rightCol = nil, nil
	}
	for _, ln := range lines {
		switch ln.col {
		case 2:
			flush()
			ordered = append(ordered, ln)
		case 1:
			rightCol = append(rightCol, ln)
		default:
			leftCol = append(leftCol, ln)
		}
	}
	flush()

	// 5) 丢弃孤立的上标/引用碎片行：1-2 字符且字号显著小于相邻正文行。
	// 典型：FRP 论文的独立单字形行 "0"（fs=6.97 vs 正文 9.96，y 悬浮于两行之间），
	// 输出后插在词间（"strength is 0 proportional"）。真实的段落短行（如
	// 「配筋率的规定。」）与正文同字号，不受影响。
	ordered = dropNoiseLines(texts, ordered)

	// 6) 判定每行与前一行是换行还是软换行续接
	for i := 1; i < len(ordered); i++ {
		prev, cur := &ordered[i-1], &ordered[i]
		if prev.col != cur.col {
			if prev.col == 2 || cur.col == 2 {
				cur.breakBefore = true
				continue
			}
			// 左栏 → 右栏：只要左栏末行排满且右栏首行不缩进，就是跨栏软换行
			cur.breakBefore, cur.stripHyphen, cur.spaceBefore =
				lineJoin(texts, prev, cur, colX1[prev.col], colX0[cur.col])
			continue
		}
		if cur.col == 2 { // 通栏行单独成段
			cur.breakBefore = true
			continue
		}
		cur.breakBefore, cur.stripHyphen, cur.spaceBefore =
			lineJoin(texts, prev, cur, colX1[cur.col], colX0[cur.col])
	}

	// 7) 下标映射回原始入参下标
	for i := range ordered {
		idx := ordered[i].idx
		for j := range idx {
			idx[j] = remap[idx[j]]
		}
	}
	return ordered
}

// dropNoiseLines 丢弃孤立的上标/图注碎片行：极短（≤2 字符）且字号显著
// 小于相邻正文行（< 0.85 倍）。邻行字号取两侧中更大的一方，避免把同字号的
// 真实短行（段落末行、章节号）误伤。
func dropNoiseLines(texts []pdf.Text, ordered []layoutLine) []layoutLine {
	fsOf := make([]float64, len(ordered))
	for i := range ordered {
		fsOf[i] = lineFontSize(texts, ordered[i])
	}
	w := 0
	for i := range ordered {
		if utf8.RuneCountInString(lineString(texts, ordered[i])) <= 2 {
			var nb float64
			if i > 0 {
				nb = fsOf[i-1]
			}
			if i+1 < len(ordered) && fsOf[i+1] > nb {
				nb = fsOf[i+1]
			}
			if nb > 0 && fsOf[i] < 0.85*nb {
				continue // 丢弃
			}
		}
		ordered[w] = ordered[i]
		w++
	}
	return ordered[:w]
}

// clusterRows 按 Y 把字形粗聚类成视觉行，返回 Y 降序。
// 与行首锚点比较而不是与上一个字形比较，避免跨行的链式漂移。
func clusterRows(texts []pdf.Text) []layoutRow {
	items := make([]int, len(texts))
	for i := range items {
		items[i] = i
	}
	sort.SliceStable(items, func(a, b int) bool {
		ta, tb := texts[items[a]], texts[items[b]]
		if ta.Y != tb.Y {
			return ta.Y > tb.Y
		}
		return ta.X < tb.X
	})
	var rows []layoutRow
	for _, i := range items {
		t := texts[i]
		n := len(rows)
		if n == 0 || math.Abs(t.Y-rows[n-1].y) > layoutLineTol {
			rows = append(rows, layoutRow{y: t.Y})
			n++
		}
		rows[n-1].idx = append(rows[n-1].idx, i)
	}
	// 上面按 (Y↓, X↑) 排序，同一行内 Y 有微小差异的字形会被 Y 主导，
	// 因此行内必须再按 X 排一次，后续的空档检测与阅读顺序都依赖这个次序。
	for i := range rows {
		idx := rows[i].idx
		sort.SliceStable(idx, func(a, b int) bool { return texts[idx[a]].X < texts[idx[b]].X })
	}
	return rows
}

// buildLines 把视觉行在 gutter 处切成左右两段，并按 (栏, Y) 合并。
// 返回的行按 Y 降序排列（同一 Y 上：通栏行在前，随后左右栏）。
func buildLines(texts []pdf.Text, rows []layoutRow, gl, gr float64) []layoutLine {
	var segs []layoutLine
	for _, r := range rows {
		segs = append(segs, splitRow(texts, r.idx, r.y, gl, gr)...)
	}
	return mergeSegments(texts, segs)
}

// splitRow 把一个视觉行的字形切成栏内行段。
func splitRow(texts []pdf.Text, idx []int, y, gl, gr float64) []layoutLine {
	one := func(seg []int, col int) []layoutLine {
		ln := layoutLine{y: y, idx: seg, col: col}
		ln.x0, ln.x1 = lineBounds(texts, seg)
		return []layoutLine{ln}
	}
	if gl == 0 && gr == 0 {
		return one(idx, 0)
	}
	mid := (gl + gr) / 2
	var left, right []int
	for _, i := range idx {
		t := texts[i]
		if t.X+t.W/2 < mid {
			left = append(left, i)
		} else {
			right = append(right, i)
		}
	}
	if len(left) == 0 {
		return one(right, 1)
	}
	if len(right) == 0 {
		return one(left, 0)
	}
	// 两侧都有字形：中间真的空出一条 gutter 才是左右栏；否则是文字连续
	// 跨过分栏通道的通栏行（标题、摘要等）。
	_, lx1 := lineBounds(texts, left)
	rx0, _ := lineBounds(texts, right)
	if rx0-lx1 >= layoutMinGutter {
		out := one(left, 0)
		return append(out, one(right, 1)...)
	}
	return one(idx, 2)
}

// mergeSegments 排序并按 (栏, Y) 合并行段。左右栏基线可能各自独立，
// 所以同一粗行切出的两段回到各自栏内再聚一次。
func mergeSegments(texts []pdf.Text, segs []layoutLine) []layoutLine {
	sort.SliceStable(segs, func(a, b int) bool {
		if segs[a].y != segs[b].y {
			return segs[a].y > segs[b].y
		}
		if segs[a].col != segs[b].col {
			return segs[a].col < segs[b].col
		}
		return segs[a].x0 < segs[b].x0
	})
	var merged []layoutLine
	for _, s := range segs {
		j := -1
		for k := len(merged) - 1; k >= 0; k-- {
			if merged[k].y-s.y > layoutLineTol {
				break
			}
			if merged[k].col == s.col {
				j = k
				break
			}
		}
		if j >= 0 {
			m := &merged[j]
			m.idx = append(m.idx, s.idx...)
			m.x0 = math.Min(m.x0, s.x0)
			m.x1 = math.Max(m.x1, s.x1)
			continue
		}
		merged = append(merged, s)
	}
	for i := range merged {
		sort.SliceStable(merged[i].idx, func(a, b int) bool {
			return texts[merged[i].idx[a]].X < texts[merged[i].idx[b]].X
		})
		fixPunctOrder(texts, merged[i].idx)
	}
	return merged
}

// fixPunctOrder 还原「标点被右移进后一个字形的格子」的排版。
// 知网 PDF 把全角标点的原点画在前一个字形的格子里（实测偏移 ≈1.8pt，而正常应紧接
// 该字右侧一格），按 X 排序会得到"可，保证"（逗号跑到"可"后面）。标点不会出现在
// 行首，与前一个字形交换即可还原成"开缝，可保证"。
func fixPunctOrder(texts []pdf.Text, idx []int) {
	for i := 1; i < len(idx); i++ {
		cur, prev := texts[idx[i]], texts[idx[i-1]]
		if !isCJKClosingPunct(cur.S) || isCJKClosingPunct(prev.S) {
			continue
		}
		if cur.X-prev.X < prev.W*0.5 {
			idx[i-1], idx[i] = idx[i], idx[i-1]
		}
	}
}

// isCJKClosingPunct 判断是否为中文行内收尾标点（不会出现在行首）。
func isCJKClosingPunct(s string) bool {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 || size != len(s) {
		return false
	}
	return strings.ContainsRune("，。、；：？！）】》」』”", r)
}

// detectGutter 找分栏通道（gutter），返回通道的 [左, 右] 边界。找不到（单栏）返回 (0, 0)。
//
// 判据是「行占用剖面」：把版心按 1pt 分箱，统计有多少视觉行有字形压在该箱上。正文双栏
// 页的 gutter 只有少数通栏行（标题、注释）跨过，是中部最深的低谷；表格的列间隙虽然也
// 反复出现，但它被正文行横穿，占用显著更高。因此不能只数「哪里有横向空档」——知网论文
// 第 3 页的试件参数表每个列间隙都重复 10 次，票数反而超过只有 9 行的正文 gutter。
//
// 定位到低谷后，再收集「与谷底相交」的行内横向空档，取交集（最大 lo / 最小 hi）
// 作为通道范围：左栏右缘两端对齐，交集的 lo 就是通道左界；右栏行首有段首缩进，
// 未缩进行的最小 hi 是通道右界。
func detectGutter(texts []pdf.Text, rows []layoutRow, pageX0, pageW float64) (float64, float64) {
	// 双栏页左右栏共用基线，两侧内容同属一个粗行，所以行数只需达到单侧的阈值；
	// 两侧是否都够由末尾的 countSideRows 校验。
	if pageW < 3*layoutMinGutter || len(rows) < layoutMinColRows {
		return 0, 0
	}

	const binW = 1.0
	nbin := int(pageW/binW) + 1
	occ := make([]int, nbin)
	type hGap struct{ lo, hi float64 }
	var gaps []hGap

	clamp := func(i int) int {
		if i < 0 {
			return 0
		}
		if i >= nbin {
			return nbin - 1
		}
		return i
	}
	for _, r := range rows {
		covered := make([]bool, nbin)
		for _, i := range r.idx {
			t := texts[i]
			a := clamp(int((t.X - pageX0) / binW))
			b := clamp(int((t.X + t.W - pageX0) / binW))
			for j := a; j <= b; j++ {
				covered[j] = true
			}
		}
		for j, c := range covered {
			if c {
				occ[j]++
			}
		}
		for k := 1; k < len(r.idx); k++ {
			prev, cur := texts[r.idx[k-1]], texts[r.idx[k]]
			if gapLo, gapHi := prev.X+prev.W, cur.X; gapHi-gapLo >= layoutMinGutter {
				gaps = append(gaps, hGap{gapLo, gapHi})
			}
		}
	}

	// 在版心中部找占用最低的连续段，作为候选通道。只看中部，避免把页边距的空白当通道。
	i0 := clamp(int(pageW * layoutGutterCenterLo / binW))
	i1 := clamp(int(pageW * layoutGutterCenterHi / binW))
	minOcc := occ[i0]
	for i := i0; i <= i1; i++ {
		if occ[i] < minOcc {
			minOcc = occ[i]
		}
	}
	bestLo, bestHi, bestW := -1, -1, 0
	for i := i0; i <= i1; {
		if occ[i] > minOcc+1 {
			i++
			continue
		}
		j := i
		for j <= i1 && occ[j] <= minOcc+1 {
			j++
		}
		if j-i > bestW {
			bestLo, bestHi, bestW = i, j, j-i
		}
		i = j
	}
	if bestLo < 0 {
		return 0, 0
	}
	segLo := pageX0 + float64(bestLo)*binW
	segHi := pageX0 + float64(bestHi)*binW

	// 与低谷段相交的行内空档取「交集」作为通道：左栏右缘两端对齐，每行 lo 一致，
	// 取最大 lo（最深的整齐边界）；右栏行首常带段首缩进，hi 逐行参差（GB 规范实测
	// 在 271-299pt 间跳动），未缩进行的首字位置才是通道真实右界，取最小 hi。
	// 旧实现要求各空档 (lo,hi) 完全一致（1pt 容差）再聚类计数，右栏缩进会让每行
	// 空档边界都不同、各成 n=1 的孤簇而漏检。交集对个别异常小空档（如居中页码）
	// 鲁棒：收缩仍发生在通道白色区间内，mid 偏移几 pt 不影响左右归属。
	var valleyGaps []hGap
	for _, g := range gaps {
		if g.hi < segLo || g.lo > segHi {
			continue
		}
		valleyGaps = append(valleyGaps, g)
	}
	// 通道宽度上限自适应：小开本扫描书（版心窄、栏距大）的栏缝可达 40-70pt，
	// 固定 36pt 会漏检；上限随版心宽度放大（15%），但远小于表格整块间隙（≥100pt），
	// 并封顶防御超大页面把表格列间隙放进候选。
	capW := math.Min(math.Max(layoutMaxGutter, 0.15*pageW), layoutGutterCapMax)
	if len(valleyGaps) < layoutMinColRows {
		return 0, 0
	}
	lo, hi := valleyGaps[0].lo, valleyGaps[0].hi
	for _, g := range valleyGaps[1:] {
		lo = math.Max(lo, g.lo)
		hi = math.Min(hi, g.hi)
	}
	if w := hi - lo; w < layoutMinGutter || w > capW {
		return 0, 0
	}
	// 两侧都要有足够的行，否则更像图表的空白而非分栏。
	// 注意双栏左右共用基线时两侧内容同属一个粗行，所以这里只要求「行内存在该侧内容」。
	if countSideRows(texts, rows, lo, true) < layoutMinColRows ||
		countSideRows(texts, rows, hi, false) < layoutMinColRows {
		return 0, 0
	}
	return lo, hi
}

// countSideRows 统计在 gutter 某一侧有内容的行数。
func countSideRows(texts []pdf.Text, rows []layoutRow, edge float64, left bool) int {
	n := 0
	for _, r := range rows {
		x0, x1 := lineBounds(texts, r.idx)
		if (left && x0 < edge) || (!left && x1 > edge) {
			n++
		}
	}
	return n
}

// lineJoin 判定 cur 行是否续接 prev 行（软换行），并给出拼接修饰。
func lineJoin(texts []pdf.Text, prev, cur *layoutLine, colRight, colLeft float64) (brk, strip, space bool) {
	prevS, curS := lineString(texts, *prev), lineString(texts, *cur)
	if prevS == "" || curS == "" {
		return true, false, false
	}
	fs := lineFontSize(texts, *cur)
	if fs <= 0 {
		fs = lineFontSize(texts, *prev)
	}
	if fs <= 0 {
		fs = 10
	}
	threshold := layoutIndentEm * fs

	// 段落首行缩进
	if colLeft > 0 && cur.x0-colLeft >= threshold {
		return true, false, false
	}
	// 上一行明显没排满 → 段落结束
	if colRight > 0 && colRight-prev.x1 >= threshold {
		return true, false, false
	}
	// 英文断词：行末连字符 + 下一行首字母 → 去掉连字符直接拼
	if strings.HasSuffix(prevS, "-") && len(prevS) > 1 {
		r, _ := utf8.DecodeRuneInString(curS)
		if isLatinWordRune(r) {
			return false, true, false
		}
	}
	// 两侧都是拉丁字母/数字 → 补空格；中文直接拼
	pr, _ := utf8.DecodeLastRuneInString(prevS)
	cr, _ := utf8.DecodeRuneInString(curS)
	return false, false, isLatinWordRune(pr) && isLatinWordRune(cr)
}

// layoutText 把页面字形按阅读顺序拼成文本：软换行合并，段落/分栏处保留 \n。
func layoutText(texts []pdf.Text) string {
	lines := layoutPage(texts)
	out := make([]string, 0, len(lines)*2)
	for i, ln := range lines {
		s := lineString(texts, ln)
		if i > 0 {
			switch {
			case ln.breakBefore:
				out = append(out, "\n")
			case ln.stripHyphen:
				if n := len(out); n > 0 && strings.HasSuffix(out[n-1], "-") {
					out[n-1] = strings.TrimSuffix(out[n-1], "-")
				}
			case ln.spaceBefore:
				out = append(out, " ")
			}
		}
		out = append(out, s)
	}
	return strings.Join(out, "")
}

// lineString 把行内字形按 X 顺序拼成文本。
// 知网等排版器不写空格字形，英文单词边界只靠几何间隙表达（实测词内≈0、词间≈0.36 字号），
// 因此在「两侧都是拉丁字母/数字 且 间隙超过阈值」处补一个空格。真有空格字形时，
// 空格自身非拉丁字符，前后两个相邻对都不会触发补写，不会出现双空格。
func lineString(texts []pdf.Text, ln layoutLine) string {
	fs := lineFontSize(texts, ln)
	var sb strings.Builder
	for k, i := range ln.idx {
		if k > 0 && fs > 0 {
			prev, cur := texts[ln.idx[k-1]], texts[i]
			if gap := cur.X - (prev.X + prev.W); gap > layoutWordGapEm*fs {
				pr, _ := utf8.DecodeLastRuneInString(prev.S)
				cr, _ := utf8.DecodeRuneInString(cur.S)
				if isLatinWordRune(pr) && isLatinWordRune(cr) {
					sb.WriteByte(' ')
				}
			}
		}
		sb.WriteString(texts[i].S)
	}
	return sb.String()
}

func lineBounds(texts []pdf.Text, idx []int) (float64, float64) {
	x0, x1 := math.Inf(1), math.Inf(-1)
	for _, i := range idx {
		t := texts[i]
		x0 = math.Min(x0, t.X)
		x1 = math.Max(x1, t.X+t.W)
	}
	return x0, x1
}

func glyphBounds(texts []pdf.Text) (float64, float64) {
	x0, x1 := math.Inf(1), math.Inf(-1)
	for _, t := range texts {
		if strings.TrimSpace(t.S) == "" {
			continue // 纯空格字形（如页眉的空白行）不代表正文边界
		}
		x0 = math.Min(x0, t.X)
		x1 = math.Max(x1, t.X+t.W)
	}
	if math.IsInf(x0, 1) {
		return 0, 0
	}
	return x0, x1
}

// lineFontSize 取行内字号的中间值，规避上标/下标等小字号干扰。
func lineFontSize(texts []pdf.Text, ln layoutLine) float64 {
	fs := make([]float64, 0, len(ln.idx))
	for _, i := range ln.idx {
		if s := texts[i].FontSize; s > 0 {
			fs = append(fs, s)
		}
	}
	if len(fs) == 0 {
		return 0
	}
	sort.Float64s(fs)
	return fs[len(fs)/2]
}

func isLatinWordRune(r rune) bool {
	return r < 0x80 && (r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
}
