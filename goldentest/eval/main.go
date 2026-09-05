// 黄金测试集评测脚本。
//
// 两个层次：
//
//	v1（离线一致性校验，默认执行）：验证黄金测试集本身是否自洽——
//	  1. golden_queries.json 引用的每条目标事实 ID 是否存在于 facts.json；
//	  2. 每条事实的原文是否能在语料（goldentest/pdf/*.pdf 解析后的文本）中逐字命中；
//	  3. 每条问题解析出的期望答案 / 论文 / 章节是否正确。
//	  任意一条标注损坏则以非零码退出（可接入 CI）。
//
//	v2（检索评测 Recall@K，需 -retrieval 开启）：真正评测 RAG 检索质量——
//	  对每条 query 用 embedding 编码问题 → 在数据库 chunks 里向量检索 topK →
//	  判断"包含目标事实文本的 chunk"是否在结果内 → 输出 Recall@K / MRR@K。
//	  前置条件：goldentest 语料已入库（可先用 -ingest 重灌）+ Ollama 在跑。
//
//	v3（问答评测，需 -answer 开启）：端到端评测回答质量——
//	  对每条 query 走真实 /ask 链路（检索 + LLM 生成），用 LLM 裁判对比期望事实判分
//	  （correct/partial/wrong），distractor 单独判"是否把干扰事实当结论"（ok/hallucinated），
//	  并根据回答的 citations 反推"检索是否命中 gold chunk"，输出端到端诊断矩阵。
//	  前置条件：语料已入库 + Ollama 在跑（需要 chat 模型）。
//
// 运行方式（在仓库根目录，密码走环境变量）：
//
//	$env:TEST_DATABASE_PASSWORD = "你的密码"
//	go run ./goldentest/eval                       # 只跑 v1 一致性校验
//	go run ./goldentest/eval -retrieval            # v1 + v2（库中须已有语料）
//	go run ./goldentest/eval -answer               # v1 + v3（需要 chat 模型）
//	go run ./goldentest/eval -ingest -retrieval -answer  # 重灌 + v1 + v2 + v3
//
// 可用参数：
//
//	-config   string   配置文件路径（默认 config/config.test.yaml）
//	-ingest           重新把 goldentest/pdf/*.pdf 摄入配置的数据库（先删同名旧文档）
//	-retrieval        运行 v2 检索评测
//	-topk     int      检索深度（默认 10，同时报告 Recall@1/3/5/topk）
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
	"paper-rag-backend/internal/model"
	"paper-rag-backend/internal/parser"
	"paper-rag-backend/internal/service"
	"paper-rag-backend/internal/store"
)

// ---------- JSON 文件结构（与 goldentest 下文件一一对应） ----------

// factsFile 只解析 atomic / distractors 两处事实原文。
// locations（论文/章节/行号）仅合成语料有、真实论文没有，且不参与评分，故不再解析。
type factsFile struct {
	Atomic      map[string]string `json:"atomic"`
	Distractors map[string]string `json:"distractors"`
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

// ---------- 评测报告 JSON 结构 ----------

// EvalReport 是整次评测的输出（写入 reports/eval_<时间戳>.json）。
type EvalReport struct {
	GeneratedAt string         `json:"generated_at"`
	Ingest      *IngestResult  `json:"ingest,omitempty"`
	V1          *V1Result      `json:"v1"`
	V2          *V2Result      `json:"v2,omitempty"`
	V3          *V3Result      `json:"v3,omitempty"`
}

type IngestResult struct {
	Database    string             `json:"database"`
	Embedding   string             `json:"embedding_model"`
	ChunkSize   int                `json:"chunk_size"`
	ChunkOverlap int               `json:"chunk_overlap"`
	DeletedOld  int                `json:"deleted_old_docs"`
	Files       []IngestFileResult `json:"files"`
}

type IngestFileResult struct {
	Filename   string `json:"filename"`
	Status     string `json:"status"` // ingested / skipped-duplicate
	DocumentID string `json:"document_id,omitempty"`
	Pages      int    `json:"pages,omitempty"`
	Chunks     int    `json:"chunks,omitempty"`
}

// V1Result 一致性校验结果。
type V1Result struct {
	Passed      bool      `json:"passed"`
	PassCount   int       `json:"pass_count"`
	Total       int       `json:"total"`
	CorpusPDFs  int       `json:"corpus_pdfs"`
	CorpusChars int       `json:"corpus_chars"`
	Queries     []V1Query `json:"queries"`
}

type V1Query struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"` // atomic / distractor / summary
	Question string   `json:"question"`
	Note     string   `json:"note,omitempty"`
	Status   string   `json:"status"` // OK / FAIL
	Broken   []string `json:"broken,omitempty"`
	Expected string   `json:"expected,omitempty"` // 原子=期望答案；干扰=期望行为说明
}

// V2Result 检索评测（Recall@K / MRR）。
type V2Result struct {
	Database      string        `json:"database"`
	Chunks        int           `json:"chunks"`
	FactsTotal    int           `json:"facts_total"`
	FactsFull     int           `json:"facts_fully_contained"`
	FactsOverlap  int           `json:"facts_overlap_only"`
	TopK          int           `json:"top_k"`
	Queries       []V2Query     `json:"queries"`
	RecallAtK     []RecallKItem `json:"recall_at_k"`
	MRR           float64       `json:"mrr"`
}

type V2Query struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Question   string   `json:"question"`
	Status     string   `json:"status"` // OK / MISS / SKIP
	Rank       int      `json:"rank,omitempty"` // 0=未命中
	Hit        []int    `json:"hit,omitempty"`  // Recall@1/3/5/topk
	TopScore   float32  `json:"top_score,omitempty"`
	SkipReason string   `json:"skip_reason,omitempty"`
	Missing    []string `json:"missing_facts,omitempty"`
}

type RecallKItem struct {
	K     int     `json:"k"`
	Hit   int     `json:"hit"`
	Total int     `json:"total"`
	Rate  float64 `json:"rate"`
}

// V3Result 问答评测（端到端回答质量 + 防幻觉）。
type V3Result struct {
	Database      string                  `json:"database"`
	ChatModel     string                  `json:"chat_model"`
	TopK          int                     `json:"top_k"`
	Queries       []V3Query               `json:"queries"`
	Atomic        *V3AtomicSummary        `json:"atomic_summary,omitempty"`
	Distractor    *V3DistractorSummary    `json:"distractor_summary,omitempty"`
}

type V3Query struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Question     string `json:"question"`
	Expected     string `json:"expected,omitempty"` // 原子=期望答案；干扰=干扰项内容
	Answer       string `json:"answer"`
	Verdict      string `json:"verdict"`
	Reason       string `json:"reason,omitempty"`
	RetrievalHit bool   `json:"retrieval_hit"`
	NumAll       int    `json:"num_all"`
	NumHit       int    `json:"num_hit"`
}

type V3AtomicSummary struct {
	Total      int        `json:"total"`
	Correct    int        `json:"correct"`
	Partial    int        `json:"partial"`
	Wrong      int        `json:"wrong"`
	Diagnostic Diagnostic `json:"diagnostic"`
}

// Diagnostic 端到端诊断矩阵：检索命中与回答正确性的组合。
type Diagnostic struct {
	RetrievalHitAnswerCorrect int `json:"retrieval_hit_answer_correct"`
	RetrievalHitAnswerWrong   int `json:"retrieval_hit_answer_wrong"`
	NoRetrievalAnswerCorrect  int `json:"no_retrieval_answer_correct"`
	NoRetrievalAnswerWrong    int `json:"no_retrieval_answer_wrong"`
}

type V3DistractorSummary struct {
	Total        int `json:"total"`
	Refused      int `json:"refused"` // ok：正确拒答
	Hallucinated int `json:"hallucinated"`
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

// progress 打印实时进度到 stdout（带时间戳，不进报告文件）。
func progress(format string, a ...any) {
	fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
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

// runConsistencyCheck 执行 v1 一致性校验，返回结构化结果。
func runConsistencyCheck(goldenDir string) (*V1Result, bool) {
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
	allFacts := buildAllFacts(facts)

	type qResult struct {
		q      goldenQuery
		broken []string
		answer string
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
			// 干扰项按设计应"不在"语料中（防幻觉测试）：在语料里反而说明设计错误。
			// 原子事实则必须逐字在语料中。
			if q.Type == "distractor" {
				if strings.Contains(normCorpus, stripWS(fact)) {
					res.broken = append(res.broken, fmt.Sprintf("干扰事实 %s 意外出现在语料中（应作为不在语料的幻觉测试项）", fid))
				}
				res.answer = fact
			} else {
				if !strings.Contains(normCorpus, stripWS(fact)) {
					res.broken = append(res.broken, fmt.Sprintf("事实 %s 在语料中逐字找不到", fid))
					continue
				}
				res.answer = fact
			}
		}
		if len(q.TargetFactIDs) == 0 {
			res.broken = append(res.broken, "未指定 target_fact_ids")
		}
		results = append(results, res)
	}

	out := &V1Result{
		CorpusPDFs:  len(pdfs),
		CorpusChars: len(corpus),
		Total:       len(results),
		Queries:     make([]V1Query, 0, len(results)),
	}
	passCnt := 0
	for _, r := range results {
		vq := V1Query{
			ID:       r.q.ID,
			Type:     r.q.Type,
			Question: r.q.Question,
			Note:     r.q.Note,
			Status:   "OK",
		}
		if len(r.broken) > 0 {
			vq.Status = "FAIL"
			vq.Broken = r.broken
		} else {
			passCnt++
		}
		if r.q.Type == "distractor" {
			vq.Expected = "模型不应把该事实当作答案（防幻觉）：应拒答/说明资料中无相关信息"
		} else {
			vq.Expected = r.answer
		}
		out.Queries = append(out.Queries, vq)
	}
	out.Passed = passCnt == len(results)
	out.PassCount = passCnt
	return out, out.Passed
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
// 返回结构化结果。用 IngestService 走真实生产流水线（解析→分块→向量化→入库）。
func ingestCorpus(ctx context.Context, cfg *config.Config, goldenDir string) (*IngestResult, error) {
	pdfs, err := filepath.Glob(filepath.Join(goldenDir, "pdf", "*.pdf"))
	if err != nil {
		return nil, err
	}
	if len(pdfs) == 0 {
		return nil, fmt.Errorf("语料目录下没有 PDF")
	}
	sort.Strings(pdfs)

	aiClient, pg, err := buildAIService(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer pg.Close()

	ingest := service.NewIngestService(aiClient, pg, cfg.Server, cfg.RAG)

	out := &IngestResult{
		Database:    cfg.Database.DBName,
		Embedding:   cfg.AI.Embedding.Model,
		ChunkSize:   cfg.RAG.ChunkSize,
		ChunkOverlap: cfg.RAG.ChunkOverlap,
	}

	// 1) 删除与 goldentest 同名文档的旧数据（可能还是修复前的脏数据），保证重灌干净。
	want := make(map[string]bool, len(pdfs))
	for _, f := range pdfs {
		want[filepath.Base(f)] = true
	}
	docs, err := ingest.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出文档失败: %w", err)
	}
	for _, d := range docs {
		if want[d.Filename] {
			if err := ingest.Delete(ctx, d.ID); err != nil {
				return nil, fmt.Errorf("删除旧文档 %s 失败: %w", d.Filename, err)
			}
			out.DeletedOld++
		}
	}

	// 2) 逐个摄入
	for _, f := range pdfs {
		fh, err := os.Open(f)
		if err != nil {
			return nil, fmt.Errorf("打开 %s: %w", f, err)
		}
		st, _ := fh.Stat()
		doc, err := ingest.Process(ctx, fh, st.Size(), filepath.Base(f))
		fh.Close()
		item := IngestFileResult{Filename: filepath.Base(f)}
		if err != nil {
			if err == service.ErrDuplicateDocument {
				item.Status = "skipped-duplicate"
				item.DocumentID = doc.ID
			} else {
				return nil, fmt.Errorf("摄入 %s 失败: %w", filepath.Base(f), err)
			}
		} else {
			item.Status = "ingested"
			item.DocumentID = doc.ID
			item.Pages = doc.PageCount
			item.Chunks = doc.ChunkCount
		}
		out.Files = append(out.Files, item)
	}
	return out, nil
}

// ---------- v2 / v3 共用工具 ----------

type chunkInfo struct {
	id    string
	docID string
	norm  string
}

// loadAllChunks 读库内所有 chunk，返回归一化内容（去空白）。
func loadAllChunks(ctx context.Context, cfg *config.Config) ([]chunkInfo, error) {
	conn, err := pgx.Connect(ctx, cfg.Database.DSN())
	if err != nil {
		return nil, fmt.Errorf("连接数据库读取 chunks 失败: %w", err)
	}
	defer conn.Close(ctx)

	var chunks []chunkInfo
	rows, err := conn.Query(ctx, `SELECT c.id, c.document_id, c.content FROM chunks c`)
	if err != nil {
		return nil, fmt.Errorf("查询 chunks 失败: %w", err)
	}
	for rows.Next() {
		var id, docID, content string
		if err := rows.Scan(&id, &docID, &content); err != nil {
			rows.Close()
			return nil, fmt.Errorf("读取 chunk 失败: %w", err)
		}
		chunks = append(chunks, chunkInfo{id: id, docID: docID, norm: stripWS(content)})
	}
	rows.Close()
	return chunks, nil
}

// buildAllFacts 合并原子 + 干扰事实，便于按 ID 查原文。
func buildAllFacts(facts factsFile) map[string]string {
	all := make(map[string]string, len(facts.Atomic)+len(facts.Distractors))
	for k, v := range facts.Atomic {
		all[k] = v
	}
	for k, v := range facts.Distractors {
		all[k] = v
	}
	return all
}

// computeGoldByFact 预计算每条事实的 gold chunks（相关 chunk）。
// 策略：优先"完整包含"事实原文的 chunk；只有该事实在库中找不到任何完整 chunk
// （即被 chunk 边界截断）时，才退回"与事实有足够长公共片段"的部分重叠 chunk。
func computeGoldByFact(chunks []chunkInfo, allFacts map[string]string) (map[string][]string, int, int) {
	goldByFact := make(map[string][]string, len(allFacts))
	fullyContained, overlapOnly := 0, 0
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
	return goldByFact, fullyContained, overlapOnly
}

// numRe 匹配数字（含小数），用于关键数字命中的辅助信号。
var numRe = regexp.MustCompile(`\d+(?:\.\d+)?`)

// keyNumberHit 统计期望事实里"不同"的数字，有多少出现在回答里。
// 仅作辅助信号供人工复核，不作判分依据（判分用 LLM 裁判）。
func keyNumberHit(expected, answer string) (all, hit int) {
	normAns := stripWS(answer)
	seen := map[string]bool{}
	for _, n := range numRe.FindAllString(stripWS(expected), -1) {
		if seen[n] {
			continue
		}
		seen[n] = true
		all++
		if strings.Contains(normAns, n) {
			hit++
		}
	}
	return all, hit
}

// judgeAnswer 用 LLM 当裁判，判定"模型回答"相对"期望答案"的对错。
// 原子问题输出 verdict: correct/partial/wrong；干扰问题输出 ok/hallucinated。
func judgeAnswer(ctx context.Context, aiClient *ai.Client, q goldenQuery, expected, answer string) (verdict, reason string) {
	var sys, user string
	if q.Type == "distractor" {
		sys = `你是严格的质量评测员。判断"模型回答"是否编造了资料中不存在的信息。
该问题对应的"干扰信息"是一条不在语料中的具体说法（用于参照模型是否可能编造它）。`
		user = fmt.Sprintf("干扰信息（不在语料中，仅作参照）:\n%s\n\n问题:\n%s\n\n模型回答:\n%s\n\n请只输出一个 JSON 对象，不要输出其他任何内容：\n{\"verdict\": \"ok\" 或 \"hallucinated\", \"reason\": \"一句话理由\"}\n- ok: 模型正确拒答，或说明资料中没有相关信息/无法确定，且未编造具体数值或细节\n- hallucinated: 模型编造了具体数值/细节作为答案（即使不是干扰信息里的那个值），例如给出了确定的数字、单位、事实性陈述", expected, q.Question, answer)
	} else {
		sys = `你是严格的质量评测员。判断"模型回答"是否准确回答了"问题"。参考标准答案（来自论文语料）。`
		user = fmt.Sprintf("标准答案:\n%s\n\n问题:\n%s\n\n模型回答:\n%s\n\n请只输出一个 JSON 对象，不要输出其他任何内容：\n{\"verdict\": \"correct\" 或 \"partial\" 或 \"wrong\", \"reason\": \"一句话理由\"}\n- correct: 准确包含正确答案的关键信息（数字/数值/实体），无错误\n- partial: 相关但缺少关键信息，或只对了一部分\n- wrong: 与标准答案矛盾，或资料中明明有答案却答“资料中没有相关信息”", expected, q.Question, answer)
	}

	reply, err := aiClient.Chat(ctx, sys, user)
	if err != nil {
		return "wrong", fmt.Sprintf("裁判调用失败: %v", err)
	}
	verdict, reason = parseVerdict(reply)
	if verdict == "" {
		return "wrong", "裁判输出无法解析"
	}
	return verdict, reason
}

// parseVerdict 从裁判回复中截取 { ... } 并解析 JSON。
func parseVerdict(reply string) (verdict, reason string) {
	start := strings.Index(reply, "{")
	end := strings.LastIndex(reply, "}")
	if start < 0 || end <= start {
		return "", ""
	}
	var v struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(reply[start:end+1]), &v); err != nil {
		return "", ""
	}
	return strings.TrimSpace(v.Verdict), strings.TrimSpace(v.Reason)
}

// runRetrievalEval 执行 v2 检索评测：问题 embedding → 向量检索 topK →
// 判断包含目标事实的 chunk 是否命中 → 输出 Recall@K / MRR@K。
func runRetrievalEval(ctx context.Context, cfg *config.Config, facts factsFile, golden goldenFile, topk int) (*V2Result, error) {
	if topk <= 0 {
		topk = 10
	}

	aiClient, pg, err := buildAIService(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer pg.Close()

	// 读库内所有 chunk，构建 归一化内容 → chunkID 的索引，用于定位"含目标事实的 chunk"。
	chunks, err := loadAllChunks(ctx, cfg)
	if err != nil {
		return nil, err
	}

	allFacts := buildAllFacts(facts)

	// 预计算每条事实的 gold chunks（相关 chunk）。
	goldByFact, fullyContained, overlapOnly := computeGoldByFact(chunks, allFacts)

	out := &V2Result{
		Database:     cfg.Database.DBName,
		Chunks:       len(chunks),
		FactsTotal:   len(allFacts),
		FactsFull:    fullyContained,
		FactsOverlap: overlapOnly,
		TopK:         topk,
		Queries:      make([]V2Query, 0, len(golden.Queries)),
	}

	for i, q := range golden.Queries {
		progress("v2 检索 %d/%d %s", i+1, len(golden.Queries), q.ID)
		vq := V2Query{ID: q.ID, Type: q.Type, Question: q.Question}

		// 干扰项按设计不在语料中，没有 gold chunk，不参与检索评测，直接跳过。
		if q.Type == "distractor" {
			vq.Status = "SKIP"
			vq.SkipReason = "干扰项按设计不在语料中，跳过检索评测"
			out.Queries = append(out.Queries, vq)
			continue
		}

		goldIDs := map[string]bool{}
		for _, fid := range q.TargetFactIDs {
			if len(goldByFact[fid]) == 0 {
				vq.Missing = append(vq.Missing, fid)
				continue
			}
			for _, id := range goldByFact[fid] {
				goldIDs[id] = true
			}
		}
		if len(goldIDs) == 0 {
			vq.Status = "SKIP"
			vq.SkipReason = "语料中找不到目标事实，建议先 -ingest（可能仍是修复前脏数据）"
			out.Queries = append(out.Queries, vq)
			continue
		}

		vec, err := aiClient.Embed(ctx, []string{q.Question})
		if err != nil {
			return out, fmt.Errorf("embedding 问题 %s 失败: %w", q.ID, err)
		}
		results, err := pg.Search(ctx, vec[0], topk, 0, store.SearchOptions{})
		if err != nil {
			return out, fmt.Errorf("检索问题 %s 失败: %w", q.ID, err)
		}
		if len(results) > 0 {
			vq.TopScore = results[0].Score
		}
		for j, r := range results {
			if goldIDs[r.Chunk.ID] {
				vq.Rank = j + 1
				break
			}
		}
		for _, k := range []int{1, 3, 5, topk} {
			hit := vq.Rank > 0 && vq.Rank <= k
			vq.Hit = append(vq.Hit, boolToInt(hit))
		}
		if vq.Rank == 0 {
			vq.Status = "MISS"
		} else {
			vq.Status = "OK"
		}
		out.Queries = append(out.Queries, vq)
	}

	// 汇总（只统计 atomic 问题）
	var atomic []V2Query
	for _, q := range out.Queries {
		if q.Type == "atomic" && q.Status == "OK" {
			atomic = append(atomic, q)
		}
	}
	if len(atomic) > 0 {
		for _, k := range []int{1, 3, 5, topk} {
			hit := 0
			for _, q := range atomic {
				// hit 数组索引：k=1→0, 3→1, 5→2, topk→3
				idx := map[int]int{1: 0, 3: 1, 5: 2, topk: 3}[k]
				hit += q.Hit[idx]
			}
			out.RecallAtK = append(out.RecallAtK, RecallKItem{K: k, Hit: hit, Total: len(atomic), Rate: float64(hit) / float64(len(atomic))})
		}
		mrr := 0.0
		for _, q := range atomic {
			if q.Rank > 0 {
				mrr += 1.0 / float64(q.Rank)
			}
		}
		out.MRR = mrr / float64(len(atomic))
	}
	return out, nil
}

// runAnswerEval 执行 v3 问答评测：对每条问题走真实 /ask 链路（检索 + LLM 生成），
// 用 LLM 裁判对比期望事实判分；distractor 单独判"是否编造/正确拒答"。
// 同时从回答的 citations 反推"检索是否命中 gold chunk"，输出端到端诊断矩阵。
func runAnswerEval(ctx context.Context, cfg *config.Config, facts factsFile, golden goldenFile) (*V3Result, error) {
	aiClient, pg, err := buildAIService(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer pg.Close()
	ask := service.NewAskService(aiClient, pg, cfg.RAG)

	chunks, err := loadAllChunks(ctx, cfg)
	if err != nil {
		return nil, err
	}
	allFacts := buildAllFacts(facts)
	goldByFact, _, _ := computeGoldByFact(chunks, allFacts)

	out := &V3Result{
		Database:  cfg.Database.DBName,
		ChatModel: cfg.AI.Chat.Model,
		TopK:      cfg.RAG.TopK,
		Queries:   make([]V3Query, 0, len(golden.Queries)),
	}

	for i, q := range golden.Queries {
		vq := V3Query{ID: q.ID, Type: q.Type, Question: q.Question}
		if len(q.TargetFactIDs) > 0 {
			vq.Expected = allFacts[q.TargetFactIDs[0]]
		}

		// 该问题对应的 gold chunk IDs
		goldIDs := map[string]bool{}
		for _, fid := range q.TargetFactIDs {
			for _, id := range goldByFact[fid] {
				goldIDs[id] = true
			}
		}

		// 走真实 /ask 链路（检索 + 生成）
		progress("v3 检索+生成 %d/%d %s ...", i+1, len(golden.Queries), q.ID)
		ans, err := ask.Ask(ctx, model.Question{Query: q.Question})
		if err != nil {
			return out, fmt.Errorf("问答问题 %s 失败: %w", q.ID, err)
		}
		vq.Answer = ans.Answer
		for _, c := range ans.Citations {
			if goldIDs[c.ChunkID] {
				vq.RetrievalHit = true
				break
			}
		}

		vq.NumAll, vq.NumHit = keyNumberHit(vq.Expected, vq.Answer)
		progress("v3 判分 %d/%d %s ...", i+1, len(golden.Queries), q.ID)
		vq.Verdict, vq.Reason = judgeAnswer(ctx, aiClient, q, vq.Expected, vq.Answer)
		progress("v3 完成 %d/%d %s -> %s", i+1, len(golden.Queries), q.ID, strings.ToUpper(vq.Verdict))
		out.Queries = append(out.Queries, vq)
	}

	// 汇总
	var atomic, distr []V3Query
	for _, r := range out.Queries {
		if r.Type == "atomic" {
			atomic = append(atomic, r)
		} else {
			distr = append(distr, r)
		}
	}
	if len(atomic) > 0 {
		sum := &V3AtomicSummary{Total: len(atomic)}
		for _, r := range atomic {
			switch r.Verdict {
			case "correct":
				sum.Correct++
				if r.RetrievalHit {
					sum.Diagnostic.RetrievalHitAnswerCorrect++
				} else {
					sum.Diagnostic.NoRetrievalAnswerCorrect++
				}
			case "partial":
				sum.Partial++
			case "wrong":
				sum.Wrong++
				if r.RetrievalHit {
					sum.Diagnostic.RetrievalHitAnswerWrong++
				} else {
					sum.Diagnostic.NoRetrievalAnswerWrong++
				}
			}
		}
		out.Atomic = sum
	}
	if len(distr) > 0 {
		sum := &V3DistractorSummary{Total: len(distr)}
		for _, r := range distr {
			if r.Verdict == "ok" {
				sum.Refused++
			} else {
				sum.Hallucinated++
			}
		}
		out.Distractor = sum
	}
	return out, nil
}

// boolToInt 把布尔值转成 0/1，用于 Recall@K 命中数组。
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
	doAnswer := flag.Bool("answer", false, "运行 v3 问答评测（端到端回答质量，需 chat 模型）")
	topk := flag.Int("topk", 10, "检索评测的 topK 深度")
	flag.Parse()

	root, err := findModuleRoot()
	if err != nil {
		fatal(err)
	}
	goldenDir := filepath.Join(root, "goldentest")

	report := &EvalReport{GeneratedAt: time.Now().Format("2006-01-02 15:04:05")}

	// v1：一致性校验（不需要数据库 / Ollama）
	progress("开始 v1 一致性校验 ...")
	facts, golden, err := loadGolden(goldenDir)
	if err != nil {
		fatal(err)
	}
	report.V1, _ = runConsistencyCheck(goldenDir)
	progress("v1 完成：%d/%d", report.V1.PassCount, report.V1.Total)

	ctx := context.Background()
	if *doIngest || *doRetrieval || *doAnswer {
		cfg, err := config.Load(filepath.Join(root, *configPath))
		if err != nil {
			fatal(fmt.Errorf("加载配置 %s 失败: %w", *configPath, err))
		}
		if *doIngest {
			progress("开始摄入语料 ...")
			ing, err := ingestCorpus(ctx, cfg, goldenDir)
			if err != nil {
				fatal(err)
			}
			progress("摄入完成：%d 篇", len(ing.Files))
			report.Ingest = ing
		}
		if *doRetrieval {
			progress("开始 v2 检索评测 ...")
			v2, err := runRetrievalEval(ctx, cfg, facts, golden, *topk)
			if err != nil {
				fatal(err)
			}
			progress("v2 完成")
			report.V2 = v2
		}
		if *doAnswer {
			progress("开始 v3 问答评测 ...")
			v3, err := runAnswerEval(ctx, cfg, facts, golden)
			if err != nil {
				fatal(err)
			}
			progress("v3 完成")
			report.V3 = v3
		}
	}

	// 写 JSON 报告
	reportDir := filepath.Join(goldenDir, "reports")
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		fatal(fmt.Errorf("创建 reports 目录: %w", err))
	}
	reportPath := filepath.Join(reportDir, "eval_"+time.Now().Format("20060102_150405")+".json")
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fatal(fmt.Errorf("序列化报告: %w", err))
	}
	if err := os.WriteFile(reportPath, data, 0o644); err != nil {
		fatal(fmt.Errorf("写报告文件: %w", err))
	}
	fmt.Printf("\n报告已保存: %s\n", reportPath)

	// 控制台精简摘要
	printSummary(report)

	if !report.V1.Passed {
		fmt.Fprintln(os.Stderr, "存在标注损坏，评测未全部通过")
		os.Exit(1)
	}
}

// printSummary 在控制台打印各层关键指标的精简摘要。
func printSummary(r *EvalReport) {
	fmt.Println("========== 摘要 ==========")
	fmt.Printf("v1 一致性校验     : %d/%d 通过\n", r.V1.PassCount, r.V1.Total)
	if r.Ingest != nil {
		fmt.Printf("语料摄入          : %d 篇（删除旧 %d 篇）\n", len(r.Ingest.Files), r.Ingest.DeletedOld)
	}
	if r.V2 != nil {
		fmt.Printf("v2 检索(原子)      : Recall@1=%v", pct(r.V2.RecallAtK, 1))
		for _, item := range r.V2.RecallAtK {
			if item.K == r.V2.TopK {
				fmt.Printf("  Recall@%d=%v", item.K, pct(r.V2.RecallAtK, item.K))
			}
		}
		fmt.Printf("  MRR@%d=%.3f\n", r.V2.TopK, r.V2.MRR)
	}
	if r.V3 != nil {
		if r.V3.Atomic != nil {
			a := r.V3.Atomic
			fmt.Printf("v3 问答(原子)      : 正确 %d/%d, 部分 %d, 错误 %d\n", a.Correct, a.Total, a.Partial, a.Wrong)
		}
		if r.V3.Distractor != nil {
			d := r.V3.Distractor
			fmt.Printf("v3 防幻觉(干扰)    : 正确拒答 %d/%d, 幻觉 %d\n", d.Refused, d.Total, d.Hallucinated)
		}
	}
}

func pct(items []RecallKItem, k int) string {
	for _, it := range items {
		if it.K == k {
			return fmt.Sprintf("%d/%d(%.0f%%)", it.Hit, it.Total, 100*it.Rate)
		}
	}
	return "-"
}
