// 黄金测试集评测脚本（v1：离线一致性校验）。
//
// 作用：验证黄金测试集本身是否自洽——
//   1. golden_queries.json 引用的每条目标事实 ID 是否存在于 facts.json；
//   2. 每条事实的原文是否能在语料（goldentest/pdf/*.pdf 解析后的文本）中逐字命中；
//   3. 每条问题解析出的期望答案 / 论文 / 章节是否正确。
// 任意一条标注损坏则以非零码退出（可接入 CI）。
//
// 运行方式（在仓库根目录）：
//   go run ./goldentest/eval
//
// 下一阶段（TODO，检索评测）：
//   对每条 query 用 embedding 编码问题 → 在数据库 chunks 里做向量检索 topK →
//   判断"包含目标事实文本的 chunk"是否在结果内 → 输出 Recall@K。
//   需要数据库已入库 goldentest 语料 + Ollama 在跑。见各 TODO 标记处。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"paper-rag-backend/internal/parser"
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

// stripWS 去除所有空白，用于"事实原文"与"解析文本"的逐字精确匹配。
func stripWS(s string) string { return wsRe.ReplaceAllString(s, "") }

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

func main() {
	root, err := findModuleRoot()
	if err != nil {
		fatal(err)
	}
	goldenDir := filepath.Join(root, "goldentest")

	// 1) 读标注文件
	factsRaw, err := os.ReadFile(filepath.Join(goldenDir, "facts.json"))
	if err != nil {
		fatal(fmt.Errorf("读取 facts.json: %w", err))
	}
	var facts factsFile
	if err := json.Unmarshal(factsRaw, &facts); err != nil {
		fatal(fmt.Errorf("解析 facts.json: %w", err))
	}

	goldenRaw, err := os.ReadFile(filepath.Join(goldenDir, "golden_queries.json"))
	if err != nil {
		fatal(fmt.Errorf("读取 golden_queries.json: %w", err))
	}
	var golden goldenFile
	if err := json.Unmarshal(goldenRaw, &golden); err != nil {
		fatal(fmt.Errorf("解析 golden_queries.json: %w", err))
	}

	// 2) 解析语料
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

	// 3) 逐条问题校验
	type qResult struct {
		q      goldenQuery
		broken []string // 未通过的检查描述
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

	// 4) 输出报告
	var sb strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&sb, format+"\n", a...) }
	w("黄金测试集一致性校验报告")
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
		} else if r.loc.Paper > 0 {
			w("  期望答案: %s", r.answer)
			w("  位置    : 论文%d / %s / L%03d", r.loc.Paper, r.loc.Section, r.loc.Line)
		}
		w("")
	}
	w("============================================================")
	w("通过 : %d / %d", passCnt, len(results))

	// 5) 写报告文件 + 打印
	reportDir := filepath.Join(goldenDir, "reports")
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		fatal(fmt.Errorf("创建 reports 目录: %w", err))
	}
	reportPath := filepath.Join(reportDir, "eval_"+time.Now().Format("20060102_150405")+".txt")
	if err := os.WriteFile(reportPath, []byte(sb.String()), 0o644); err != nil {
		fatal(fmt.Errorf("写报告文件: %w", err))
	}
	fmt.Print(sb.String())
	fmt.Printf("\n报告已保存: %s\n", reportPath)

	// TODO(下一阶段)：检索评测。embedding 编码 query → store.Search 取 topK →
	// 检查命中 chunk 内容是否包含目标事实 → 统计 Recall@K。
	// 需测试库已入库 goldentest 语料（见 tests/integration）且 Ollama 可用。
	// 建议新增 -retrieval 开关：未提供数据库密码 / Ollama 不可用时跳过。

	if passCnt != len(results) {
		fmt.Fprintln(os.Stderr, "存在标注损坏，评测未全部通过")
		os.Exit(1)
	}
}
