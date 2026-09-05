// 黄金测试集评测脚本。
//
// 两个层次：
//   v1（离线一致性校验，默认执行）：验证黄金测试集本身是否自洽——
//     1. golden_queries.json 引用的每条目标事实 ID 是否存在于 facts.json；
//     2. 每条事实的原文是否能在语料（goldentest/pdf/*.pdf 解析后的文本）中逐字命中；
//     3. 每条问题解析出的期望答案 / 论文 / 章节是否正确。
//     任意一条标注损坏则以非零码退出（可接入 CI）。
//
//   v2（检索评测 Recall@K，需 -retrieval 开启）：真正评测 RAG 检索质量——
//     对每条 query 用 embedding 编码问题 → 在数据库 chunks 里向量检索 topK →
//     判断"包含目标事实文本的 chunk"是否在结果内 → 输出 Recall@K / MRR@K。
//     前置条件：goldentest 语料已入库（可先用 -ingest 重灌）+ Ollama 在跑。
//
// 运行方式（在仓库根目录，密码走环境变量）：
//   $env:TEST_DATABASE_PASSWORD = "你的密码"
//   go run ./goldentest/eval                       # 只跑 v1 一致性校验
//   go run ./goldentest/eval -retrieval            # v1 + v2（库中须已有语料）
//   go run ./goldentest/eval -ingest -retrieval    # 先重灌语料，再 v1 + v2
//
// 可用参数：
//   -config   string   配置文件路径（默认 config/config.test.yaml）
//   -ingest           重新把 goldentest/pdf/*.pdf 摄入配置的数据库（先删同名旧文档）
//   -retrieval        运行 v2 检索评测
//   -topk     int      检索深度（默认 10，同时报告 Recall@1/3/5/topk）
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"paper-rag-backend/internal/ai"
	"paper-rag-backend/internal/config"
	"paper-rag-backend/internal/parser"
	"paper-rag-backend/internal/service"
	"paper-rag-backend/internal/store"
)

// ---------- JSON 文件结构（与 goldentest 下文件一一对应） ----------

type factLocation struct {
	Paper   int    `json:"paper"`
	Section string `json:"section"`
	Line    int    `json:"line"`
}

type factsFile struct {
	Atomic      map[string]string       `json:"atomic"`
	Distractors map[string]string       `json:"distractors"`
	Locations   map[string]factLocation `json:"locations"`
}

type goldenQuery struct {
	ID            string   `json:"id"`
	Question      string   `json:"question"`
	TargetFactIDs []string `json:"target_fact_ids"`
	Type          string   `json:"type"`
	Note          string   `json:"note"`
}

type goldenFile struct {
	SchemaVersion int           `json:"schema_version"`
	Queries       []goldenQuery `json:"queries"`
}

var wsRe = regexp.MustCompile(`\s+`)

// stripWS 去除所有空白，用于"事实原文"与"解析文本/库内 chunk"的逐字精确匹配。
func stripWS(s string) string { return wsRe.ReplaceAllString(s, "") }

// overlapMin 判定"chunk 承载了事实的一部分"的最小公共片段长度。
const overlapMin = 24

// factOverlapsChunk 判断 chunk 是否与事实有足够长的公共片段，用于"事实被 chunk 边界截断"的兜底。
// 完全包含由 strings.Contains 处理；这里负责：事实的任何连续 overlapMin（至少 len/2）个字符
// 出现在 chunk 里，即认为该 chunk 与该事实相关（装着被切开的半个事实）。
func factOverlapsChunk(chunkNorm, factNorm string) bool {
	n := overlapMin
	if half := len(factNorm) / 2; n > half {
		n = half
	}
	if n < 1 {
		n = 1
	}
	for i := 0; i+n <= len(factNorm); i++ {
		if strings.Contains(chunkNorm, factNorm[i:i+n]) {
			return true
		}
	}
	return false
}

// findModuleRoot 向上查找含 go.mod 的目录（仓库根），
// 使脚本无论在哪个目录下执行都能定位 goldentest/。
func findModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("找不到 go.mod，请确认在仓库目录下运行")
		}
		dir = parent
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(2)
}

// loadGolden 读入 facts.json 与 golden_queries.json。
func loadGolden(goldenDir string) (factsFile, goldenFile, error) {
	var facts factsFile
	factsRaw, err := os.ReadFile(filepath.Join(goldenDir, "facts.json"))
	if err != nil {
		return facts, goldenFile{}, fmt.Errorf("读取 facts.json: %w", err)
	}
	if err := json.Unmarshal(factsRaw, &facts); err != nil {
		return facts, goldenFile{}, fmt.Errorf("解析 facts.json: %w", err)
	}

	var golden goldenFile
	goldenRaw, err := os.ReadFile(filepath.Join(goldenDir, "golden_queries.json"))
	if err != nil {
		return facts, golden, fmt.Errorf("读取 golden_queries.json: %w", err)
	}
	if err := json.Unmarshal(goldenRaw, &golden); err != nil {
		return facts, golden, fmt.Errorf("解析 golden_queries.json: %w", err)
	}
	return facts, golden, nil
}

// parseCorpus 解析语料目录下所有 PDF，返回拼接文本与按文件名排序的清单。
func parseCorpus(pdfDir string) (string, []string, error) {
	pdfs, err := filepath.Glob(filepath.Join(pdfDir, "*.pdf"))
	if err != nil {
		return "", nil, err
	}
	if len(pdfs) == 0 {
		return "", nil, fmt.Errorf("语料目录 %s 下没有 PDF", pdfDir)
	}
	sort.Strings(pdfs)

	var corpus strings.Builder
	for _, f := range pdfs {
		data, err := os.ReadFile(f)
		if err != nil {
			return "", nil, fmt.Errorf("读取 %s: %w", f, err)
		}
		doc, err := parser.ParsePDF(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return "", nil, fmt.Errorf("解析 %s: %w", f, err)
		}
		corpus.WriteString(strings.Join(doc.Pages, "\n"))
	}
	return corpus.String(), pdfs, nil
}

// runConsistencyCheck 执行 v1 一致性校验，返回报告文本与是否全部通过。
func runConsistencyCheck(goldenDir string) (string, bool) {
	facts, golden, err := loadGolden(goldenDir)
	if err != nil {
		fatal(err)
	}
	corpus, pdfs, err := parseCorpus(filepath.Join(goldenDir, "pdf"))
	if err != nil {
		fatal(err)
	}
	normCorpus := stripWS(corpus)

	// 全部事实（原子 + 干扰）便于按 ID 查原文
	allFacts := make(map[string]string, len(facts.Atomic)+len(facts.Distractors))
	for k, v := range facts.Atomic {
		allFacts[k] = v
	}
	for k, v := range facts.Distractors {
		allFacts[k] = v
	}

	type qResult struct {
		q      goldenQuery
		broken []string
		answer string
		loc    factLocation
	}
	var results []qResult
	for _, q := range golden.Queries {
		res := qResult{q: q}
		for _, fid := range q.TargetFactIDs {
			fact, ok := allFacts[fid]
			if !ok {
				res.broken = append(res.broken, fmt.Sprintf("目标事实 %s 不在 facts.json 中", fid))
				continue
			}
			if !strings.Contains(normCorpus, stripWS(fact)) {
				res.broken = append(res.broken, fmt.Sprintf("事实 %s 在语料中逐字找不到", fid))
				continue
			}
			if loc, ok := facts.Locations[fid]; ok {
				res.loc = loc
				res.answer = fact
			}
		}
		if len(q.TargetFactIDs) == 0 {
			res.broken = append(res.broken, "未指定 target_fact_ids")
		}
		results = append(results, res)
	}

	var sb strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&sb, format+"\n", a...) }
	w("黄金测试集一致性校验报告（v1）")
	w("============================================================")
	w("生成时间 : %s", time.Now().Format("2006-01-02 15:04:05"))
	w("语料     : %d 篇 PDF，解析文本 %d 字符", len(pdfs), len(corpus))
	w("问题数   : %d", len(results))
	w("")

	passCnt := 0
	for _, r := range results {
		status := "OK "
		if len(r.broken) > 0 {
			status = "FAIL"
		} else {
			passCnt++
		}
		w("[%s] %s", status, r.q.ID)
		w("  问题    : %s", r.q.Question)
		w("  类型    : %s", r.q.Type)
		if len(r.q.Note) > 0 {
			w("  备注    : %s", r.q.Note)
		}
		if len(r.broken) > 0 {
			w("  未通过  :")
			for _, b := range r.broken {
				w("    - %s", b)
			}
		}
		if r.q.Type == "distractor" {
			w("  期望    : 模型不应把该事实当作答案（防幻觉）")
		} else {
			// 期望答案只依赖事实原文，与是否有 location 无关
			// （真实论文可不提供 locations，照样能看到期望答案）。
			if r.answer != "" {
				w("  期望答案: %s", r.answer)
			}
			if r.loc.Paper > 0 {
				w("  位置    : 论文%d / %s / L%03d", r.loc.Paper, r.loc.Section, r.loc.Line)
			}
		}
		w("")
	}
	w("============================================================")
	w("通过 : %d / %d", passCnt, len(results))
	return sb.String(), passCnt == len(results)
}

// buildAIService 按配置构建 AI 客户端与 PostgreSQL 存储。
func buildAIService(ctx context.Context, cfg *config.Config) (*ai.Client, *store.PostgresStore, error) {
	pg, err := store.NewPostgresStore(ctx, cfg.Database.DSN(), cfg.Database.MaxConns)
	if err != nil {
		return nil, nil, fmt.Errorf("连接数据库 %s 失败: %w", cfg.Database.DBName, err)
	}
	return ai.NewClient(cfg.AI), pg, nil
}

// ingestCorpus 把 goldentest/pdf/*.pdf 重新摄入配置的数据库（先删除同文件名的旧文档），
// 返回摘要文本。用 IngestService 走真实生产流水线（解析→分块→向量化→入库）。
func ingestCorpus(ctx context.Context, cfg *config.Config, goldenDir string) (string, error) {
	pdfs, err := filepath.Glob(filepath.Join(goldenDir, "pdf", "*.pdf"))
	if err != nil {
		return "", err
	}
	if len(pdfs) == 0 {
		return "", fmt.Errorf("语料目录下没有 PDF")
	}
	sort.Strings(pdfs)

	aiClient, pg, err := buildAIService(ctx, cfg)
	if err != nil {
		return "", err
	}
	defer pg.Close()

	ingest := service.NewIngestService(aiClient, pg, cfg.Server, cfg.RAG)

	// 1) 删除与 goldentest 同名文档的旧数据（可能还是修复前的脏数据），保证重灌干净。
	want := make(map[string]bool, len(pdfs))
	for _, f := range pdfs {
		want[filepath.Base(f)] = true
	}
	docs, err := ingest.List(ctx)
	if err != nil {
		return "", fmt.Errorf("列出文档失败: %w", err)
	}
	deleted := 0
	for _, d := range docs {
		if want[d.Filename] {
			if err := ingest.Delete(ctx, d.ID); err != nil {
				return "", fmt.Errorf("删除旧文档 %s 失败: %w", d.Filename, err)
			}
			deleted++
		}
	}

	// 2) 逐个摄入
	var sb strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&sb, format+"\n", a...) }
	w("黄金测试集语料摄入（-ingest）")
	w("============================================================")
	w("删除旧文档 : %d 篇", deleted)
	w("配置       : 数据库 %s / embedding %s / chunk_size %d / overlap %d",
		cfg.Database.DBName, cfg.AI.Embedding.Model, cfg.RAG.ChunkSize, cfg.RAG.ChunkOverlap)
	for _, f := range pdfs {
		fh, err := os.Open(f)
		if err != nil {
			return sb.String(), fmt.Errorf("打开 %s: %w", f, err)
		}
		st, _ := fh.Stat()
		doc, err := ingest.Process(ctx, fh, st.Size(), filepath.Base(f))
		fh.Close()
		if err != nil {
			if err == service.ErrDuplicateDocument {
				w("跳过 %s : 内容已存在（%s）", filepath.Base(f), doc.ID)
				continue
			}
			return sb.String(), fmt.Errorf("摄入 %s 失败: %w", filepath.Base(f), err)
		}
		w("已摄入 %s : id=%s 页数=%d chunk=%d", filepath.Base(f), doc.ID, doc.PageCount, doc.ChunkCount)
	}
	return sb.String(), nil
}

// runRetrievalEval 执行 v2 检索评测：问题 embedding → 向量检索 topK →
// 判断包含目标事实的 chunk 是否命中 → 输出 Recall@K / MRR@K。
func runRetrievalEval(ctx context.Context, cfg *config.Config, goldenDir string, facts factsFile, golden goldenFile, topk int) (string, error) {
	if topk <= 0 {
		topk = 10
	}

	aiClient, pg, err := buildAIService(ctx, cfg)
	if err != nil {
		return "", err
	}
	defer pg.Close()

	// 读库内所有 chunk，构建 归一化内容 → chunkID 的索引，用于定位"含目标事实的 chunk"。
	conn, err := pgx.Connect(ctx, cfg.Database.DSN())
	if err != nil {
		return "", fmt.Errorf("连接数据库读取 chunks 失败: %w", err)
	}
	defer conn.Close(ctx)

	type chunkInfo struct {
		id    string
		docID string
		norm  string
	}
	var chunks []chunkInfo
	rows, err := conn.Query(ctx, `SELECT c.id, c.document_id, c.content FROM chunks c`)
	if err != nil {
		return "", fmt.Errorf("查询 chunks 失败: %w", err)
	}
	for rows.Next() {
		var id, docID, content string
		if err := rows.Scan(&id, &docID, &content); err != nil {
			rows.Close()
			return "", fmt.Errorf("读取 chunk 失败: %w", err)
		}
		chunks = append(chunks, chunkInfo{id: id, docID: docID, norm: stripWS(content)})
	}
	rows.Close()

	allFacts := make(map[string]string, len(facts.Atomic)+len(facts.Distractors))
	for k, v := range facts.Atomic {
		allFacts[k] = v
	}
	for k, v := range facts.Distractors {
		allFacts[k] = v
	}

	// 预计算每条事实的 gold chunks（相关 chunk）。
	// 策略：优先"完整包含"事实原文的 chunk；只有该事实在库中找不到任何完整 chunk
	// （即被 chunk 边界截断）时，才退回"与事实有足够长公共片段"的部分重叠 chunk。
	goldByFact := make(map[string][]string, len(allFacts))
	fullyContained := 0
	overlapOnly := 0
	for fid, text := range allFacts {
		ft := stripWS(text)
		var fullIDs, overlapIDs []string
		for _, c := range chunks {
			if strings.Contains(c.norm, ft) {
				fullIDs = append(fullIDs, c.id)
			} else if factOverlapsChunk(c.norm, ft) {
				overlapIDs = append(overlapIDs, c.id)
			}
		}
		if len(fullIDs) > 0 {
			fullyContained++
			goldByFact[fid] = fullIDs
		} else if len(overlapIDs) > 0 {
			overlapOnly++
			goldByFact[fid] = overlapIDs
		}
	}

	var sb strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&sb, format+"\n", a...) }
	w("检索评测报告（v2）：Recall@K / MRR@K")
	w("============================================================")
	w("数据库     : %s", cfg.Database.DBName)
	w("库内 chunk : %d 个", len(chunks))
	w("事实覆盖   : %d/%d 在单个 chunk 内完整命中；%d/%d 仅部分重叠（跨 chunk 边界）",
		fullyContained, len(allFacts), overlapOnly, len(allFacts))
	w("topK 深度  : %d", topk)
	w("问题数     : %d", len(golden.Queries))
	w("")

	// 逐条问题评测
	type evalRow struct {
		q      goldenQuery
		ok     bool     // 是否能评估（gold chunk 存在）
		miss   []string // 找不到 gold chunk 的事实
		rank   int      // 首个命中 rank（0=未命中）
		hit    []int    // 各 K 是否命中
		topScore float32
	}
	var rows2 []evalRow
	for _, q := range golden.Queries {
		row := evalRow{q: q}
		goldIDs := map[string]bool{}
		for _, fid := range q.TargetFactIDs {
			if len(goldByFact[fid]) == 0 {
				row.miss = append(row.miss, fid)
				continue
			}
			for _, id := range goldByFact[fid] {
				goldIDs[id] = true
			}
		}
		if len(goldIDs) == 0 {
			rows2 = append(rows2, row)
			continue
		}
		row.ok = true

		vec, err := aiClient.Embed(ctx, []string{q.Question})
		if err != nil {
			return sb.String(), fmt.Errorf("embedding 问题 %s 失败: %w", q.ID, err)
		}
		results, err := pg.Search(ctx, vec[0], topk, 0, store.SearchOptions{})
		if err != nil {
			return sb.String(), fmt.Errorf("检索问题 %s 失败: %w", q.ID, err)
		}
		if len(results) > 0 {
			row.topScore = results[0].Score
		}
		for i, r := range results {
			if goldIDs[r.Chunk.ID] {
				row.rank = i + 1
				break
			}
		}
		for _, k := range []int{1, 3, 5, topk} {
			hit := row.rank > 0 && row.rank <= k
			row.hit = append(row.hit, boolToInt(hit))
		}
		rows2 = append(rows2, row)
	}

	// 报告逐条结果
	for _, r := range rows2 {
		status := "OK "
		detail := ""
		if !r.ok {
			status = "SKIP"
			detail = "语料中找不到目标事实，建议先 -ingest（可能仍是修复前脏数据）"
		} else if r.rank == 0 {
			status = "MISS"
			detail = fmt.Sprintf("top%d 内未命中（最高分 %.3f）", topk, r.topScore)
		} else {
			detail = fmt.Sprintf("命中@rank=%d 最高分=%.3f", r.rank, r.topScore)
		}
		w("[%s] %s  %s", status, r.q.ID, detail)
		w("  问题  : %s", r.q.Question)
		if r.ok {
			w("  目标  : %v  Recall@1/3/5/%d = %v/%v/%v/%v",
				r.q.TargetFactIDs, topk, r.hit[0], r.hit[1], r.hit[2], r.hit[3])
		} else {
			w("  缺失  : %v", r.miss)
		}
		if r.q.Type == "distractor" {
			w("  类型  : distractor（检索命中属正常，不参与原子事实指标汇总）")
		}
		w("")
	}

	// 汇总（只统计 atomic 问题）
	var atomic []evalRow
	for _, r := range rows2 {
		if r.q.Type == "atomic" && r.ok {
			atomic = append(atomic, r)
		}
	}
	w("============================================================")
	if len(atomic) == 0 {
		w("无可统计的原子问题（全部跳过或缺失）")
	} else {
		hits := func(kIdx int) int {
			n := 0
			for _, r := range atomic {
				n += r.hit[kIdx]
			}
			return n
		}
		mrr := 0.0
		for _, r := range atomic {
			if r.rank > 0 {
				mrr += 1.0 / float64(r.rank)
			}
		}
		mrr /= float64(len(atomic))
		ks := []int{1, 3, 5, topk}
		w("原子问题 : %d 条（不含 distractor）", len(atomic))
		for i, k := range ks {
			w("Recall@%-2d = %d/%d = %.1f%%", k, hits(i), len(atomic), 100*float64(hits(i))/float64(len(atomic)))
		}
		w("MRR@%-2d   = %.4f", topk, mrr)
	}
	return sb.String(), nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func main() {
	configPath := flag.String("config", "config/config.test.yaml", "评测用配置文件路径")
	doIngest := flag.Bool("ingest", false, "先把 goldentest/pdf 语料重新摄入数据库，再评测")
	doRetrieval := flag.Bool("retrieval", false, "运行 v2 检索评测（Recall@K）")
	topk := flag.Int("topk", 10, "检索评测的 topK 深度")
	flag.Parse()

	root, err := findModuleRoot()
	if err != nil {
		fatal(err)
	}
	goldenDir := filepath.Join(root, "goldentest")

	// v1：一致性校验（不需要数据库 / Ollama）
	facts, golden, err := loadGolden(goldenDir)
	if err != nil {
		fatal(err)
	}
	v1Report, v1OK := runConsistencyCheck(goldenDir)

	var report strings.Builder
	report.WriteString(v1Report)

	ctx := context.Background()
	if *doIngest || *doRetrieval {
		cfg, err := config.Load(filepath.Join(root, *configPath))
		if err != nil {
			fatal(fmt.Errorf("加载配置 %s 失败: %w", *configPath, err))
		}
		if *doIngest {
			s, err := ingestCorpus(ctx, cfg, goldenDir)
			if err != nil {
				fatal(err)
			}
			report.WriteString("\n")
			report.WriteString(s)
			report.WriteString("\n")
		}
		if *doRetrieval {
			s, err := runRetrievalEval(ctx, cfg, goldenDir, facts, golden, *topk)
			if err != nil {
				fatal(err)
			}
			report.WriteString("\n")
			report.WriteString(s)
			report.WriteString("\n")
		}
	}

	// 写报告文件 + 打印
	reportDir := filepath.Join(goldenDir, "reports")
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		fatal(fmt.Errorf("创建 reports 目录: %w", err))
	}
	reportPath := filepath.Join(reportDir, "eval_"+time.Now().Format("20060102_150405")+".txt")
	if err := os.WriteFile(reportPath, []byte(report.String()), 0o644); err != nil {
		fatal(fmt.Errorf("写报告文件: %w", err))
	}
	fmt.Print(report.String())
	fmt.Printf("\n报告已保存: %s\n", reportPath)

	if !v1OK {
		fmt.Fprintln(os.Stderr, "存在标注损坏，评测未全部通过")
		os.Exit(1)
	}
}
