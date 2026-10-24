package parser

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/ledongthuc/pdf"

	"paper-rag-backend/internal/logger"
)

// BlockKind 区分页面内容块的类型。
type BlockKind int

const (
	// BlockText 是普通文本块。
	BlockText BlockKind = iota
	// BlockTable 是表格块，Content 为 Markdown 表格文本。
	BlockTable
	// BlockFormula 是公式块，Content 为扁平文本（行间用 \n 分隔），
	// 后续由服务层交给公式 OCR 还原成 LaTeX。
	BlockFormula
)

// Block 是页面内容的一个有序块：普通文本、表格（Markdown）或公式（扁平文本）。
// 公式块额外携带 PDF 坐标边界（用于后续渲染截图 OCR）；其他块类型这些字段为 0。
type Block struct {
	Kind    BlockKind
	Content string
	// 公式块坐标（PDF 点，Y 向上）。仅 Kind==BlockFormula 时有意义。
	Left, Right, Top, Bot float64
}

// ParsedDocument 是 PDF 解析后的结果，按页保留文本与块流。
type ParsedDocument struct {
	Title     string
	PageCount int
	Pages     []string  // Pages[i] 为第 i 页的文本（表格以 Markdown 表示，向后兼容）
	Blocks    [][]Block // Blocks[i] 为第 i 页的有序块流（text/table 按版面顺序交替）
}

// ParsePDF 从 reader 解析 PDF，返回按页划分的文本与块流。
func ParsePDF(r io.ReaderAt, size int64) (*ParsedDocument, error) {
	end, err := trimTrailingJunk(r, size)
	if err != nil {
		return nil, fmt.Errorf("无法读取 PDF: %w", err)
	}
	reader, err := pdf.NewReader(io.NewSectionReader(r, 0, end), end)
	if err != nil {
		return nil, fmt.Errorf("无法读取 PDF: %w", err)
	}

	pageCount := reader.NumPage()

	// 预扫描：找出"解码不可信的字体"（NUL 任一出现 / FFFD 占比过半，坏字体整层跳过，见 scanBadFonts）。
	// 扫描件 OCR 垃圾层（如 HiddenVertOCR）是 NUL 的主要来源。
	badFonts := scanBadFonts(reader, pageCount)
	if len(badFonts) > 0 {
		names := make([]string, 0, len(badFonts))
		for n := range badFonts {
			names = append(names, n)
		}
		sort.Strings(names)
		logger.Warn("parse.bad_fonts_skipped", "fonts", strings.Join(names, ","))
	}

	doc := &ParsedDocument{
		Title:     inferTitle(reader, badFonts),
		PageCount: pageCount,
		Pages:     make([]string, 0, pageCount),
		Blocks:    make([][]Block, 0, pageCount),
	}

	for i := 1; i <= pageCount; i++ {
		pageStart := time.Now()
		blocks, err := pageBlocks(reader.Page(i), badFonts)
		if err != nil {
			return nil, fmt.Errorf("解析第 %d 页失败: %w", i, err)
		}
		doc.Blocks = append(doc.Blocks, blocks)
		text := pageTextFromBlocks(blocks)
		doc.Pages = append(doc.Pages, text)
		logger.Debug("parse.page_done", "page", i, "total", pageCount, "blocks", len(blocks), "chars", len([]rune(text)), "duration_ms", time.Since(pageStart).Milliseconds())
	}
	return doc, nil
}

// scanBadFonts 预扫描全部页面，收集"解码不可信的字体"集合。
//
// 依据：正常排版字体几乎从不会产生解码失败字符——失败字符只有两种来源：
// NUL(\x00)：ToUnicode CMap 把字形码映射到 U+0000。典型案例：扫描件 OCR 的
//
//	HiddenVertOCR 竖排垃圾层（NUL+FFFD+乱码混入正文，且 NUL 会被 PostgreSQL
//	text 类型拒绝 → SQLSTATE 22021）。
//
// U+FFFD 替换符：字体无 ToUnicode 映射或映射表损坏，大片字形解不出来。
//
//	典型案例：SCIRP 论文 PDF（黄金评测集语料）正文整页全是 \uFFFD，quote 校验
//	全部 DROPPED。此类字体按"FFFD 占比"判定——某字体在全文档范围 FFFD 占比
//	过半即视为坏层整层剔除（正常字体只会在个别字符上偶发 FFFD，远达不到一半）。
//
// 注意：不能按字体名黑名单（各 OCR 工具的层命名各不相同：Hidden*OCR / ABBYY / Adobe…），
// 按"内容质量信号（NUL 或 FFFD 占比）"判定更通用，对正常排版 PDF 零影响。
func scanBadFonts(reader *pdf.Reader, pageCount int) map[string]bool {
	bad := make(map[string]bool)
	type fontStat struct {
		total, fail int
	}
	stats := make(map[string]*fontStat)
	for i := 1; i <= pageCount; i++ {
		for _, t := range reader.Page(i).Content().Text {
			st := stats[t.Font]
			if st == nil {
				st = &fontStat{}
				stats[t.Font] = st
			}
			st.total++
			if strings.ContainsRune(t.S, '\x00') {
				bad[t.Font] = true // NUL：任一出现即坏（还会炸 PostgreSQL）
				st.fail++
				continue
			}
			if strings.ContainsRune(t.S, '\uFFFD') {
				st.fail++
			}
		}
	}
	for name, st := range stats {
		if st.total > 0 && float64(st.fail)/float64(st.total) > 0.5 {
			bad[name] = true
		}
	}
	return bad
}

// filterBadFonts 从字形列表剔除坏字体（解码不可信字体）的字形。
func filterBadFonts(text []pdf.Text, badFonts map[string]bool) []pdf.Text {
	if len(badFonts) == 0 {
		return text
	}
	out := make([]pdf.Text, 0, len(text))
	for _, t := range text {
		if badFonts[t.Font] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// pageTextFromBlocks 把页面块流拼接成整页文本（块间空行分隔），
// 供 Pages 字段向后兼容使用。
func pageTextFromBlocks(blocks []Block) string {
	var sb strings.Builder
	for i, b := range blocks {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(b.Content)
	}
	return normalizeText(sb.String())
}

// pageBlocks 提取页面的有序块流：普通文本块 + 表格块（Markdown）+ 公式块（扁平文本）。
// 块按版面顺序排列（从上到下），表格块与公式块整体独立，不与其他文本混排。
// 优先级：表格 > 公式 > 普通文本（同一区域的字形先归表格，再归公式）。
// badFonts 是扫描件 OCR 垃圾层字体集合（产生过 NUL），其字形在入口处整层剔除。
func pageBlocks(p pdf.Page, badFonts map[string]bool) (blocks []Block, err error) {
	defer func() {
		if r := recover(); r != nil {
			blocks = nil
			err = fmt.Errorf("%v", r)
		}
	}()

	content := p.Content()
	content.Text = filterBadFonts(content.Text, badFonts) // 坏字体字形整层剔除（NUL 来源）
	tables := detectTables(content)
	formulas := DetectFormulas(content)
	if len(tables) == 0 && len(formulas) == 0 {
		text, err := pageText(p, badFonts)
		if err != nil {
			return nil, err
		}
		// 必须过 normalizeText（与下方 flushText 路径一致）：剥 \x00 / 非法 UTF-8 / 折叠空白。
		// 曾踩坑：快速路径漏了它，扫描件 OCR 垃圾层的 NUL 进 chunk → PostgreSQL 拒绝 0x00
		// （SQLSTATE 22021，GB 50010-2010 全篇入不了库）。
		return []Block{{Kind: BlockText, Content: normalizeText(text)}}, nil
	}

	// 公式区域字形 -> 公式下标（原文下标，与 content.Text 一致）
	formulaOfIdx := make(map[int]int, len(formulas))
	for fi, f := range formulas {
		for _, idx := range f.Idx {
			formulaOfIdx[idx] = fi
		}
	}

	// 排序遍历：layoutPage 给出分栏/分行后的阅读顺序，idx 仍是 content.Text 的原始下标
	lines := layoutPage(content.Text)

	var textBuf strings.Builder
	flushText := func() {
		if s := normalizeText(textBuf.String()); s != "" {
			blocks = append(blocks, Block{Kind: BlockText, Content: s})
		}
		textBuf.Reset()
	}

	tableRendered := make([]bool, len(tables))
	formulaRendered := make([]bool, len(formulas))
	for li, ln := range lines {
		for k, idx := range ln.idx {
			t := content.Text[idx]
			ti := -1
			for j := range tables {
				if t.Y >= tables[j].bot-1 && t.Y <= tables[j].top+1 {
					ti = j
					break
				}
			}
			if ti >= 0 {
				if !tableRendered[ti] {
					flushText()
					blocks = append(blocks, Block{Kind: BlockTable, Content: tables[ti].md})
					tableRendered[ti] = true
				}
				continue
			}
			if fi, ok := formulaOfIdx[idx]; ok {
				if !formulaRendered[fi] {
					flushText()
					f := formulas[fi]
					blocks = append(blocks, Block{
						Kind:    BlockFormula,
						Content: f.Text,
						Left:    f.Left,
						Right:   f.Right,
						Top:     f.Top,
						Bot:     f.Bot,
					})
					formulaRendered[fi] = true
				}
				continue
			}
			if k == 0 && li > 0 && textBuf.Len() > 0 {
				switch {
				case ln.breakBefore:
					textBuf.WriteByte('\n')
				case ln.stripHyphen:
					if s := textBuf.String(); strings.HasSuffix(s, "-") {
						textBuf.Reset()
						textBuf.WriteString(strings.TrimSuffix(s, "-"))
					}
				case ln.spaceBefore:
					textBuf.WriteByte(' ')
				}
			}
			textBuf.WriteString(t.S)
		}
	}
	flushText()
	return blocks, nil
}

// pageText 用库的 Content()（带坐标的逐字文本）按阅读顺序重建页面文本。
//
// 为什么不用 GetPlainText：它处理 PDF 换行运算符 T* 时，会把换行字节 0x0A
// 交给当前字体的 ToUnicode 解码器。而 reportlab 嵌入的 SimHei 子集字体恰好把
// 0x0A 映射成真实汉字（常规子集是"页"，粗体子集是"端/磺/境/稀/书"等），于是
// 每行末尾、段落折行处都被混入一个假字符（见 AGENTS.md 的踩坑记录）。
// Content() 内部正确跟踪文本矩阵（Tm/Td/T*），返回带 X/Y 坐标的逐字文本，
// 这里交给 layoutPage 还原分栏/分行/软换行，不产生任何假字符。
func pageText(p pdf.Page, badFonts map[string]bool) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			text = ""
			err = fmt.Errorf("%v", r)
		}
	}()

	content := p.Content()
	content.Text = filterBadFonts(content.Text, badFonts) // 坏字体字形整层剔除（NUL 来源）
	return layoutText(content.Text), nil
}

// trimTrailingJunk 返回 PDF 有效部分的结束位置。
// 部分来源（如知网 CNKI）会在 %%EOF 之后追加 XML 元数据，而解析库
// ledongthuc/pdf 要求文件末尾正好是 %%EOF（只读最后 100 字节做 HasSuffix 校验），
// 否则报 missing %%EOF。这里从尾部往前找最后一个 %%EOF，若其后还有内容则
// 截断到该位置；找不到 %%EOF 则原样返回 size（交给解析库报正常错误）。
func trimTrailingJunk(r io.ReaderAt, size int64) (int64, error) {
	const scanWindow = 1 << 20 // 最多往回扫描 1MB，覆盖常规追加的元数据
	start := size - scanWindow
	if start < 0 {
		start = 0
	}
	buf := make([]byte, size-start)
	n, err := r.ReadAt(buf, start)
	if err != nil && err != io.EOF {
		return size, err
	}
	buf = buf[:n]

	idx := bytes.LastIndex(buf, []byte("%%EOF"))
	if idx < 0 {
		return size, nil
	}
	end := start + int64(idx) + int64(len("%%EOF"))
	if end >= size {
		return size, nil
	}
	return end, nil
}

// inferTitle 从元数据或第一页前几行猜测文档标题。
func inferTitle(r *pdf.Reader, badFonts map[string]bool) string {
	// PDF 规范里 Info 在 trailer 下（trailer /Info /Title），不是 Root 下的 Info。
	if t := r.Trailer().Key("Info").Key("Title"); !t.IsNull() {
		if s := t.Text(); strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	if r.NumPage() >= 1 {
		text, err := pageText(r.Page(1), badFonts)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(normalizeText(text)), "\n")
			for _, l := range lines {
				l = strings.TrimSpace(l)
				if len(l) > 5 && len(l) <= 200 {
					return l
				}
			}
		}
	}
	return ""
}

// normalizeText 清洗解析出来的原始文本：合并行尾换行、去重多余空白。
// 同时剥离 \x00 空字节与非法 UTF-8 序列——部分字形（如上标 ²）会被解析库
// 映射成空字节，而 PostgreSQL 不接受 \x00，不清理会导致入库报错。
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ToValidUTF8(s, "")
	var sb strings.Builder
	prevBlank := false
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if !prevBlank {
				sb.WriteByte('\n')
			}
			prevBlank = true
			continue
		}
		prevBlank = false
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return strings.TrimSpace(sb.String())
}
