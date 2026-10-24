# 黄金测试集（Golden Test Set）

用真实论文语料定量评估 RAG 的检索与生成质量，替代"肉眼感觉"式验收。

## 思路

**三层数据模型**（`data/` 下三个 JSON）：

```
docs.json            语料台账：12 篇真实论文/规范（R01-R12），含 filename + sha256 + 版面属性
  ↓ 每篇抽 5-10 条
facts.json           证据原子库：101 条 verbatim fact（quote 必须逐字摘自 PDF 文本层）
  ↓ 题目引用 fact 作为证据
golden_queries.json  题目：问题 + 关联 fact + 答案 keypoints + 判分规则
```

**双轴难度分级**——每道题打两个正交标签，支持按难度切片统计指标：

- 检索轴 R1-R4：R1 单文档单证据 → R2 单文档多证据/表格取数 → R3 多证据合成 → R4 跨文档对比/冲突
- 生成轴 A1-A4：A1 直接抽取 → A2 数值计算/单位换算 → A3 多证据合成 → A4 推理与冲突归因

**quote 铁律**：fact 的 quote 字段必须程序化从 PDF 文本层截取，不得手打、不得修正 OCR 错字——它同时是"证据"和"检索正确性锚点"。校验时用统一归一化（NFKC → 去空白 → 连字符/波浪号统一），否则中文语料大面积假失败。

**两道闸**：

1. **G0 数据门禁**（`g0_check.py`）：页级校验（quote 落在 PDF 原文页）+ chunk 级校验（quote 落在入库后的 chunk 内）。三态：OK / SPLIT（跨相邻 chunk 可拼接）/ MISSING（门禁失败）。MISSING 说明解析器把这句切碎了——检索再强也救不回来。
2. **评测 runner**（`run_eval.py`）：调真实 `/search` + `/ask` 接口，全库检索（不限定文档），输出：
   - 检索轴：page Recall@K / MRR（主指标，chunk 级受切分影响太碎，作诊断指标）
   - 生成轴：keypoints 命中率 + 引用页召回 + 数值容差判定

**消融开关**：后端支持请求级三态开关 `hybrid`（纯向量 vs 向量+全文 RRF 融合）与 `rerank`（cross-encoder 精排），runner 用 `--hybrid/--rerank` 传入，组成 2×2 消融矩阵。

## 本地跑法

前置：起 test 环境后端（`config/config.test.yaml`，模板见 `config/config.test.example.yaml`，端口 8081，库 `paper_rag_test`），语料 PDF 放入 `corpus/` 并已通过上传接口入库。

```powershell
# 1. 环境变量（敏感信息不进仓库）
$env:GOLDEN_USER_ID = "<admin_users 名单内的 user_id>"
$env:GOLDEN_DSN = "host=localhost port=5432 user=postgres password=... dbname=paper_rag_test"

cd goldentest/tools

# 2. G0 数据门禁：页级（离线，需 pymupdf）；--db 追加 chunk 级（需数据库）
python -X utf8 g0_check.py
python -X utf8 g0_check.py --db --dsn $env:GOLDEN_DSN

# 3. 评测（需后端已启动）
python -X utf8 run_eval.py                                   # 完整：检索 + 生成
python -X utf8 run_eval.py --no-ask                          # 只跑检索轴（快）

# 4. 消融矩阵（hybrid × rerank 四档）
python -X utf8 run_eval.py --no-ask --hybrid false --rerank false --out eval_A.json
python -X utf8 run_eval.py --no-ask --hybrid true  --rerank false --out eval_B.json
python -X utf8 run_eval.py --no-ask --hybrid true  --rerank true  --out eval_C.json
python -X utf8 run_eval.py --no-ask --hybrid false --rerank true  --out eval_D.json
```

结果写入 `results/`，含逐题 top-K 命中明细。

## 目录

```
goldentest/
├── README.md            # 本文档
├── AGENT_PROMPT.md      # fact 标注 agent 提示词（加新语料时用子 agent 批量标注）
├── data/                # 入 git：docs.json / facts.json / golden_queries.json
├── tools/               # g0_check.py / run_eval.py
├── corpus/              # gitignore：PDF 语料（sha256 校验见 docs.json，恢复方法见其 README）
└── results/             # gitignore：评测产物
```

## 当前状态（2026-10-25）

- 12 篇语料 / 101 条 fact 页级 G0 全过 / 5 道题结构通过
- chunk 级 G0：OK 71 / MISSING 30（坏文本层 11 待 OCR、表格识别 8、公式区域 6、GB 交织 4、未定位 1；明细 `results/G0_MISSING_REPORT.md`）
- 首轮消融（5 题）：page R@10 四档均 0.867；MRR 0.750 → rerank 后 0.767；chunk R@10 恒定 0.367（瓶颈在解析器表格断行）
- 欠账：样本量不足以分辨消融档位，扩题到 15-20 道（重点 R2/R3 字面敏感型）是下一步
