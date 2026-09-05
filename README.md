# Paper RAG Backend

基于 Go + Gin 的论文 RAG（检索增强生成）后端服务。上传 PDF → 解析 → 分块 → 向量化 → 入库；针对用户问题做向量检索，并用命中的片段生成带引用的回答。

## 技术栈

- **Go** 1.21+（开发环境为 1.26）
- **Gin** Web 框架
- **Ollama 本地模型**（默认）：Embedding 用 `bge-m3`，Chat 用 `qwen3:8b`，全部本地运行、无需联网与 API Key
- **OpenAI 兼容接口**：`base_url` 可切换任意 OpenAI 兼容服务（DeepSeek / Qwen / Ollama / OpenAI）
- **PostgreSQL 18 + pgvector**（默认存储），也可切回内存实现便于开发
- **ledongthuc/pdf** 纯 Go PDF 文本提取

## 目录结构

```
cmd/server/            # 程序入口
config/                # 配置文件（config.example.yaml 为模板）
db/init.sql            # 建库建表脚本
internal/config/       # 配置加载（支持 ${ENV} 占位符）
internal/model/        # 领域模型
internal/parser/       # PDF 解析
internal/chunker/      # 文本分块
internal/ai/           # Embedding + Chat 客户端
internal/store/        # 存储接口 + 内存/PostgreSQL 实现
internal/service/      # 摄入流水线 / RAG 问答
internal/httpapi/      # Gin 路由与处理器
tests/                 # 测试：unit/ 单元测试、integration/ 集成测试（连测试库）
goldentest/            # 黄金测试集：语料(pdf/)、事实库、问题集、评测脚本
uploads/               # 上传的 PDF 落盘目录
temp.py                # 黄金语料生成器：生成 PDF 与 facts.json
```

## 快速开始

### 1. 准备依赖

- **PostgreSQL 18+**，并安装 **pgvector** 扩展（Windows 需用 nmake 编译，见 pgvector 官方文档）
- **Ollama**，并拉取模型：

```bash
ollama pull bge-m3        # embedding（多语言，1024 维）
ollama pull qwen3:8b      # chat（中文对话）
```

### 2. 准备配置

```bash
cp config/config.example.yaml config/config.yaml
```

编辑 `config/config.yaml`，至少确认：

```yaml
ai:
  base_url: "http://localhost:11434/v1"   # 默认 Ollama；embedding/chat 可各自覆盖
  api_key: "ollama"                       # Ollama 不需要 key，填任意值
  embedding:
    base_url: ""                          # 可选：embedding 单独用别的地址，留空则用 ai.base_url
    api_key: ""                           # 可选：留空则用 ai.api_key
    model: "bge-m3"
    dimension: 1024                       # ⚠️ 必须与数据库 VECTOR(n) 一致
  chat:
    base_url: ""                          # 可选：chat 单独用别的地址（如 OpenAI），留空则用 ai.base_url
    api_key: ""                           # 可选：chat 单独用别的 key（如 ${OPENAI_API_KEY}），留空则用 ai.api_key
    model: "qwen3:8b"
database:
  type: "postgres"                        # 仅支持 postgres
  user: "postgres"
  password: "${POSTGRES_PASSWORD}"        # 本地数据库密码
```

数据库密码用环境变量注入（避免明文入库）：

```powershell
$env:POSTGRES_PASSWORD = "你的数据库密码"
```

> ⚠️ **维度一致性**：`ai.embedding.dimension` 必须与 `chunks.embedding` 列的 `VECTOR(n)` 一致。
> 更换 embedding 模型后，必须同步改配置维度 + 改数据库列维度，并**重新上传所有文档**（不同模型的向量不兼容）。

### 3. 准备数据库

执行 `db/init.sql` 建表（建库 `paper_rag` 后在其中执行）：

```bash
psql -U postgres -d paper_rag -f db/init.sql
```

或直接在 Navicat 中粘贴执行 `db/init.sql` 内容。

### 4. 启动服务

```bash
go run ./cmd/server -config config/config.yaml
```

默认监听 `0.0.0.0:8080`，启动后访问 `GET /api/v1/health` 验证。

### 5. 使用

**上传 PDF 并入库（支持一次多个，`-F` 可重复）**

```bash
curl -X POST http://localhost:8080/api/v1/documents \
  -F "file=@paper-a.pdf" \
  -F "file=@paper-b.pdf"
```

返回**逐条带状态**的结果（HTTP 恒为 200；单文件失败不影响其他文件）：

```json
{
    "total": 2,
    "success_count": 1,
    "failed_count": 1,
    "results": [
        {
            "filename": "paper-a.pdf",
            "success": true,
            "document_id": "xxx",
            "page_count": 182,
            "size_bytes": 12761948,
            "duration_ms": 3500
        },
        {
            "filename": "paper-b.pdf",
            "success": false,
            "document_id": "yyy",
            "page_count": 0,
            "size_bytes": 5678,
            "duration_ms": 120,
            "error": "PDF 解析失败: ..."
        }
    ]
}
```

- `success: true` 的文件已入库，`document_id` 可用于后续指定文档检索
- `success: false` 的文件带 `error` 说明原因，其他文件照常入库

**上传去重**：上传时对每个文件计算 SHA-256 并存入 `documents.content_hash`。若库中已有**内容相同**的文档（且非 `failed` 状态），该文件会被标记为 `duplicate: true` 并跳过，不会重复入库（`duplicate_count` 单独统计，不计入失败）：

```json
{
    "total": 1,
    "success_count": 0,
    "failed_count": 0,
    "duplicate_count": 1,
    "results": [
        {
            "filename": "paper-a.pdf",
            "success": false,
            "duplicate": true,
            "document_id": "已有文档的 id",
            "error": "文档已存在（内容相同），已跳过重复上传"
        }
    ]
}
```

> 说明：`failed` 状态的文档不参与去重（内容从未入库），允许重新上传重试。

**旧库升级**（已有数据库需手动执行一次，新库执行 `db/init.sql` 已包含）：

```sql
ALTER TABLE documents ADD COLUMN content_hash VARCHAR(64);
CREATE UNIQUE INDEX idx_documents_content_hash ON documents(content_hash) WHERE status <> 'failed';
ALTER TABLE documents ADD COLUMN user_id VARCHAR(64) NOT NULL DEFAULT 'local';
```

> `user_id` 为文档归属者，当前单用户模式固定为 `'local'`，为将来多用户权限隔离铺路（不做登录鉴权）。

**针对文档提问（RAG）**

```bash
curl -X POST http://localhost:8080/api/v1/ask \
  -H "Content-Type: application/json" \
  -d '{"query": "这篇论文提出的方法是什么？"}'
```

返回包含回答与引用（来源文件、页码、相似度）：

```json
{
    "answer": "根据文档内容，本文提出了一种基于Transformer的方法... [1]",
    "citations": [
        {
            "chunk_id": "xxx",
            "document_id": "xxx",
            "filename": "paper.pdf",
            "page": 3,
            "score": 0.82,
            "snippet": "本文提出..."
        }
    ],
    "total_chunks": 5,
    "latency_ms": 3200,
    "created_at": "2026-08-17T15:40:00+08:00"
}
```

## API 一览

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/health` | 健康检查 |
| POST | `/api/v1/documents` | 上传一个或多个 PDF（multipart 字段 `file` 可重复），逐个解析并向量化入库 |
| GET | `/api/v1/documents` | 列出全部文档 |
| GET | `/api/v1/documents/:id` | 查询文档详情 |
| DELETE | `/api/v1/documents/:id` | 删除文档及向量（级联删除 chunks）|
| POST | `/api/v1/ask` | 提问，返回基于检索片段的回答与引用 |

`/ask` 请求体：

```json
{
    "query": "问题内容",
    "top_k": 5,
    "document_ids": ["doc-id-1", "doc-id-2"],
    "user_id": "local"
}
```

`query` 必填；`top_k` 可选（检索片段数，默认取配置 `rag.top_k`）；`document_ids` 可选（限定在指定文档范围内检索，不传则全库检索）；`user_id` 可选（检索只命中该用户上传的文档，不传默认 `local`）。

**指定文档对比提问**示例：先 `GET /api/v1/documents` 拿到两篇论文的 id，再限定范围提问：

```bash
curl -X POST http://localhost:8080/api/v1/ask \
  -H "Content-Type: application/json" \
  -d '{"query": "对比这两篇论文的核心观点", "document_ids": ["<doc-a-id>", "<doc-b-id>"]}'
```

## 黄金测试集（goldentest/）

用于定量评估 RAG 的**检索质量（v2）**与**端到端回答质量（v3）**，替代"肉眼感觉"式验收。语料为 5 篇合成论文（**PDF 固定，不再重新生成**），事实库 + 问题集人工维护：

```
goldentest/
├── pdf/                  # 语料：5 篇合成论文 PDF（固定）
├── parsed/               # （预留）解析后的规范文本
├── reports/              # 评测报告（eval_<时间戳>.json）
├── facts.json            # 事实库：atomic 30 条（与 PDF 逐字一致）+ distractors 20 条（不在语料）
├── golden_queries.json   # 问题集：30 条原子问题（带前提）+ 20 条干扰问题
└── eval/main.go          # 评测脚本
```

**两类标注的分工**：
- `facts.json` → `atomic`：原子事实，**逐字与 PDF 一致**（v1/v2 靠内容匹配定位，是"语义锚点"）；`distractors`：干扰项，**语料中不存在的假说法**（专测模型会不会编造）。
- `golden_queries.json`：原子问题带"前提/语境"（如"在澄江实验站的土壤水文学研究中…"），答案即对应事实；干扰问题期望模型**拒答/不编造**。

**跑评测**（密码走环境变量）：

```powershell
$env:TEST_DATABASE_PASSWORD = "你的数据库密码"
go run ./goldentest/eval                          # v1 一致性校验（无需 DB/Ollama）
go run ./goldentest/eval -retrieval               # + v2 检索 Recall@K/MRR
go run ./goldentest/eval -answer                  # + v3 问答（端到端 + 防幻觉，需 chat 模型）
go run ./goldentest/eval -ingest -retrieval -answer  # 重灌语料 + 全部
```

**报告为 JSON**（`reports/eval_<时间戳>.json`），含 `ingest / v1 / v2 / v3` 四段 + 逐条问题结果；控制台只打印关键指标摘要。

- **v1 一致性校验**：原子事实必须逐字在语料；干扰项必须**不在**语料（缺席校验）。任意标注损坏以非零码退出（可接 CI）。
- **v2 检索评测**：问题 embedding → 库内向量检索 topK → 判断含目标事实的 chunk 是否命中 → `Recall@1/3/5/10` + `MRR`；干扰项跳过（不在语料）。`-ingest` 会先删除同名旧文档再重灌。
- **v3 问答评测**：走真实 `/ask` 链路（检索 + LLM 生成），LLM 裁判判分——原子问题 `correct/partial/wrong`，干扰问题 `ok/hallucinated`（是否编造）；并据回答的 citations 反推检索命中，输出**端到端诊断矩阵**（定位问题在检索还是生成）。

## 测试

测试用例按依赖分两类（黑盒测试，仅使用导出接口）：

| 目录 | 内容 | 依赖 |
|------|------|------|
| `tests/unit/` | 单元测试（分块/配置/余弦/PDF/HTTP 层） | 无 |
| `tests/integration/` | PostgreSQL 集成测试 | 真实数据库 |

`tests/unit/` 里的 `TestParseRealPDFs*` 会解析 `goldentest/pdf/` 下的真实 PDF，并用 `goldentest/facts.json` 校验 50 条事实能逐字命中（黄金集未提交时自动跳过）。

```bash
go test ./...        # 跑全部（集成测试未设密码自动跳过）
go test ./tests/unit # 只跑单元测试
```

PostgreSQL 集成测试连**独立的测试库** `paper_rag_test`（配置见 `config/config.test.yaml`），需先建库建表 + 设置密码：

```powershell
$env:TEST_DATABASE_PASSWORD = "你的数据库密码"
go test ./tests/integration -run TestPostgresStore -v
```

> 测试库初始化：在 Navicat 新建数据库 `paper_rag_test`，然后执行 `db/init.sql`（已含最新表结构，无需 ALTER）。

## 存储层

`internal/store` 定义了统一 `Store` 接口，当前只有一个实现：

| 实现 | 配置 `database.type` | 特点 |
|------|---------------------|------|
| `PostgresStore` | `postgres` | 生产推荐，pgvector 向量检索，数据持久化 |

接口抽象保证后续若接新数据库（Qdrant、Milvus 等）只需新增实现，业务代码零改动。

## 设计要点

- **依赖注入**：handler → service → store 接口 + ai 客户端，全部由 main 装配
- **向量检索**：pgvector `<=>` 余弦距离 + HNSW 索引，毫秒级返回
- **引用溯源**：chunk 带页码，回答带 `[n]` 编号引用
- **状态追踪**：文档 `pending / ready / failed` 三态，失败保留原因
- **嵌入模型一致性**：文档与提问必须用同一 embedding 模型（同坐标系）；chat 模型可任意替换
