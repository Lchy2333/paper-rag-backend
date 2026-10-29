# Paper RAG Backend

基于 Go + Gin 的论文 RAG（检索增强生成）后端服务。上传 PDF → 解析 → 分块 → 向量化 → 入库；针对用户问题做向量检索，并用命中的片段生成带引用的回答。

## 技术栈

- **Go** 1.21+（开发环境为 1.26）
- **Gin** Web 框架
- **SiliconFlow 云 API**（默认）：Embedding 用 `BAAI/bge-m3`（1024 维），Chat 用 `deepseek-ai/DeepSeek-V3`，公式 OCR 用 `deepseek-ai/DeepSeek-OCR`（视觉模型），全部走 OpenAI 兼容 API
- **OpenAI 兼容接口**：`base_url` 可切换任意 OpenAI 兼容服务（SiliconFlow / DeepSeek / 火山方舟 / OpenAI / Ollama 本地）
- **PostgreSQL 18 + pgvector**（唯一存储实现，pgvector + tsvector + pg_trgm 混合检索）
- **ledongthuc/pdf** 纯 Go PDF 文本提取（含**框线表格识别 → Markdown**、**公式区域检测**）

## 目录结构

```
cmd/server/            # 程序入口（另有 cmd/formula_ocr、cmd/formula_ocr_pdf 公式调试工具）
config/                # 配置文件（config.example.yaml 为模板）
db/init.sql            # 建库建表脚本
internal/config/       # 配置加载（支持 ${ENV} 占位符）
internal/logger/       # log/slog 封装（统一结构化日志，log.level 控制）
internal/model/        # 领域模型
internal/parser/       # PDF 解析（文本 + 框线表格转 Markdown + 公式区域检测 + 双栏版面重建）
internal/chunker/      # 分块（文本按字符切，表格按"表头+N 行"自包含切，公式整块独立）
internal/ai/           # Embedding + Chat + 公式 OCR（图片截图 → LaTeX）+ Rerank 客户端
internal/render/       # PDF 渲染（PyMuPDF）按公式坐标裁剪截图
internal/segment/      # 中文分词（gojieba，纯 Go），供 tsvector 全文检索
internal/store/        # 存储接口 + PostgreSQL 实现（pgvector + tsvector 混合检索）
internal/service/      # 摄入流水线 / RAG 问答 / 纯检索 / 精排
internal/httpapi/      # Gin 路由与处理器（含鉴权中间件）
internal/worker/       # 异步摄入 worker 池（DB 队列 + 门铃）
tests/                 # 测试：unit/ 单元测试、integration/ 集成测试（连测试库）
goldentest/            # 黄金测试集：语料(corpus/)、事实库、问题集、评测脚本
uploads/               # 上传的 PDF 落盘目录
```

## 快速开始

### 1. 准备依赖

- **PostgreSQL 18+**，并安装 **pgvector** 扩展（Windows 需用 nmake 编译，见 pgvector 官方文档）
- **SiliconFlow API Key**（embedding / chat / OCR / rerank 全走硅基流动）：

```bash
# 注册 SiliconFlow 拿 API Key：https://siliconflow.cn/
# 配置环境变量：SILICONFLOW_API_KEY=<你的 key>（也可用 ${ENV} 注入 config.yaml）
pip install pymupdf       # 公式截图渲染（MuPDF 内核，可选，需配置 render.python_cmd）
```

> 💡 2026-10 起已从本地 Ollama 全面迁移到 SiliconFlow 云 API（embedding `BAAI/bge-m3` 维度 1024 与
> `VECTOR(1024)` 一致，无需改库）。若想自托管，可把 `base_url` 指回 `http://localhost:11434/v1`。

### 2. 准备配置

```bash
cp config/config.example.yaml config/config.yaml
```

编辑 `config/config.yaml`，至少确认：

```yaml
ai:
  base_url: "https://api.siliconflow.cn/v1"   # 默认 SiliconFlow；embedding/chat 可各自覆盖
  api_key: "${SILICONFLOW_API_KEY}"           # 硅基流动 API Key（环境变量注入）
  embedding:
    base_url: ""                              # 可选：embedding 单独用别的地址，留空则用 ai.base_url
    api_key: ""                               # 可选：留空则用 ai.api_key
    model: "BAAI/bge-m3"
    dimension: 1024                           # ⚠️ 必须与数据库 VECTOR(n) 一致
  chat:
    base_url: ""                              # 可选：chat 单独用别的地址（如火山方舟），留空则用 ai.base_url
    api_key: ""                               # 可选：chat 单独用别的 key，留空则用 ai.api_key
    model: "deepseek-ai/DeepSeek-V3"
  formula:                                    # 可选：公式 OCR（公式区域截图 → LaTeX）
    base_url: "https://api.siliconflow.cn/v1" # 可选：留空则用 ai.base_url
    api_key: "${SILICONFLOW_API_KEY}"
    model: "deepseek-ai/DeepSeek-OCR"         # 视觉模型；识别失败则公式保持扁平文本入库

render:                                   # 可选：PDF 渲染（公式截图），需先 pip install pymupdf
  python_cmd: "python"                    # Python 可执行文件；留空则公式退回扁平文本
  dpi: 150                                # 渲染分辨率
  work_dir: ""                            # 渲染临时目录，留空用系统临时目录
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

**上传 PDF（支持一次多个，`-F` 可重复）—— 异步摄入语义（2026-10 起）**

```bash
curl -X POST http://localhost:8080/api/v1/documents \
  -F "file=@paper-a.pdf" \
  -F "file=@paper-b.pdf"
```

上传接口只做**接收段**（校验 / 落盘 / 去重 / 建 pending 记录）**秒回**；
慢活（解析 / 公式 OCR / 向量化）由后台 worker 异步处理（`rag.ingest_workers` 控制并发，默认 2）。
前端拿 `document_id` **轮询 `GET /documents/:id`** 看处理进度：`pending`（排队）→ `processing`（处理中）→ `ready`（可检索）/ `failed`（带 `error` 原因）。

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
            "status": "pending",
            "document_id": "xxx",
            "page_count": 0,
            "size_bytes": 12761948,
            "duration_ms": 45
        },
        {
            "filename": "paper-b.pdf",
            "success": false,
            "size_bytes": 5678,
            "duration_ms": 3,
            "error": "不支持的文件类型，仅支持 PDF"
        }
    ]
}
```

- `success: true` = **已接收并入队**（`status: "pending"`），处理中客户端断连不影响入库；
  页数 / chunk 数入库后由 `GET /documents/:id` 返回（接收阶段未知，`page_count` 恒为 0）
- `success: false` = 接收失败（校验不过 / 落盘失败），未建记录，带 `error` 说明原因
- 处理失败的文档 `status: "failed"` 且 `error` 带原因；同内容允许重新上传重试

**轮询处理进度**：

```bash
curl http://localhost:8080/api/v1/documents/xxx
# {"document": {"id": "xxx", "status": "processing", ...}}   → 还在处理
# {"document": {"id": "xxx", "status": "ready", "chunk_count": 210, ...}}  → 可检索
# {"document": {"id": "xxx", "status": "failed", "error": "...", ...}}     → 处理失败
```

> worker 崩溃恢复：进程重启时 `processing` 的文档自动重置回 `pending` 续跑（幂等清场，不产生重复 chunks）。

**上传去重**：接收段对每个文件计算 SHA-256 并存入 `documents.content_hash`。若**同一用户**已上传**内容相同**的文档（且非 `failed` 状态），该文件会被标记为 `duplicate: true` 并当场跳过（重复检查在同步段，不占队列）：

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
            "status": "ready",
            "document_id": "已有文档的 id",
            "error": "文档已存在（内容相同），已跳过重复上传"
        }
    ]
}
```

> 说明：`failed` 状态的文档不参与去重（内容从未入库），允许重新上传重试。

**旧库升级**（多用户迁移，2026-10；新库执行 `db/init.sql` 已包含全部）：
- 去重索引由**全局** `(content_hash)` 改为**按用户** `(user_id, content_hash)`——用户 B 可上传用户 A 已有的相同文件（各自归属、各自分块参数）
- 旧单用户模式的 `local` 归属已统一迁移为 `admin`；`documents.user_id` 默认值改 `'admin'`

```sql
-- 旧库升级脚本（幂等）
UPDATE documents SET user_id = 'admin' WHERE user_id = 'local';
ALTER TABLE documents ALTER COLUMN user_id SET DEFAULT 'admin';
DROP INDEX IF EXISTS idx_documents_content_hash;
CREATE UNIQUE INDEX idx_documents_user_hash ON documents(user_id, content_hash) WHERE status <> 'failed';
```

> `user_id` 为文档归属者，由 `X-User-ID` 头注入（Agent 认证后传递）；admin 为全库超级用户（`server.admin_users` 配置）。

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

## Docker 部署

本项目已容器化（`Dockerfile`：Go 多阶段构建 + PyMuPDF 公式截图）。**完整编排（postgres + rag + agent + fronted）在 [paper-rag-deploy](https://github.com/Lchy2333/paper-rag-deploy) 仓**，用 docker-compose 一键拉起：

```bash
# 1. clone 四个仓（保持并列目录）
git clone .../paper-rag-agent.git
git clone .../paper-rag-backend.git
git clone .../paper-rag-fronted.git
git clone .../paper-rag-deploy.git
# 2. 配密钥 env 文件（见 deploy 仓 README）
# 3. 拉起
cd paper-rag-deploy && docker compose up -d --build
```

> Dockerfile 关键点：构建依赖全走国内源（GOPROXY / pip / npm / apt）；
> `third_party/`（go.mod replace 本地 fork）须在 go mod download 前 COPY；
> gojieba 需 cgo（去 CGO_ENABLED=0 + 运行时装 libstdc++6）且运行时复制词典到编译路径。
> 踩坑详见 [deploy 仓《部署避坑指南》](https://github.com/Lchy2333/paper-rag-deploy/blob/main/避坑指南.md)。

## API 一览

> **鉴权**（除 `/health` 外全部接口）：
> - `X-API-Key`：服务级鉴权（Agent → RAG），`config.yaml server.api_key`，空 = 开发模式不启用
> - `X-User-ID`：用户身份（Agent 认证后注入），**缺失返回 401**；所有文档必须有归属
>
> **多用户隔离**：search / 文档 CRUD 按 `X-User-ID` 归属过滤；`server.admin_users` 配置的
> 超级用户（Agent 的 user_id，通常是 UUID）不过滤（全库）；`ask` **仅管理员可调**。

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/health` | 健康检查（豁免鉴权） |
| POST | `/api/v1/connections/test` | 手动测试 LLM 连通性 |
| POST | `/api/v1/documents` | 上传一个或多个 PDF（multipart 字段 `file` 可重复）。**异步摄入**：接收段秒回（去重/校验当场反馈），解析/向量化由后台 worker 处理，轮询 `GET /documents/:id` 看 status |
| GET | `/api/v1/documents` | 列出文档（普通用户只看自己的，admin 全库） |
| GET | `/api/v1/documents/:id` | 查询文档详情（越权/不存在 404） |
| POST | `/api/v1/documents/:id/rechunk` | 删除该文档全部 chunk 后用新分块参数重新分块入库 |
| DELETE | `/api/v1/documents/:id` | 删除文档及向量（越权/不存在 404） |
| POST | `/api/v1/ask` | 提问，返回基于检索片段的回答与引用（**仅管理员**，全局检索） |
| POST | `/api/v1/search` | 纯检索（不调 LLM），按归属过滤，Agent 默认入口。**SSE 流式**（2026-10 起）：检索过程逐阶段推 `progress` 事件，完成后推 `result`（见下节「`/search` SSE 协议」） |

**上传时的可选分块参数**（作用于本次上传的全部文件，缺省用配置 `rag.chunk_size` / `rag.chunk_overlap`）：

| 表单字段 | 说明 |
|---------|------|
| `chunk_size` | 覆盖默认分块大小（字符数） |
| `chunk_overlap` | 覆盖默认重叠字符数（必须小于 chunk_size） |

```bash
curl -X POST http://localhost:8080/api/v1/documents \
  -F "file=@论文.pdf" \
  -F "chunk_size=400" \
  -F "chunk_overlap=50"
```

**重分块**（先上传拿到文档 id，再换分块参数重跑）：请求体为可选的 JSON，空 body 用文档当前参数。

```bash
curl -X POST http://localhost:8080/api/v1/documents/<doc-id>/rechunk \
  -H "Content-Type: application/json" \
  -d '{"chunk_size": 400, "chunk_overlap": 50}'
```

> 重分块会重新解析 PDF 并重跑公式 OCR/向量化（慢），期间旧 chunk 一直可用；解析/向量化失败时不动旧数据。

**连接测试**：`kind` 必填（`embedding` / `chat` / `formula`），发一次最小请求验证 base_url + api_key + model 是否可用。formula 会发一张内置测试图给视觉模型 OCR（只看连通性，不看识别结果）。服务器会拦截本机/内网地址（SSRF 防护），结果返回 `{ok, latency_ms, reply, error}`。

```bash
curl -X POST http://localhost:8080/api/v1/connections/test \
  -H "Content-Type: application/json" \
  -d '{"kind": "chat", "base_url": "https://api.siliconflow.cn/v1", "api_key": "sk-xxx", "model": "deepseek-ai/DeepSeek-V3"}'
```

`/ask` / `/search` 请求体（**`user_id` 已从 body 移除**，改由 `X-User-ID` 头提供）：

```json
{
    "query": "问题内容",
    "top_k": 5,
    "document_ids": ["doc-id-1", "doc-id-2"],
    "rerank": true
}
```

`query` 必填；`top_k` 可选（检索片段数，默认取配置 `rag.top_k`）；`document_ids` 可选（限定在指定文档范围内检索，不传则全库检索）；`rerank` 可选（请求级精排开关：**不传 = 跟随配置 `ai.rerank.enabled`**；`true` = 强制开启重排序（要求已配置 `ai.rerank.model`）；`false` = 强制关闭）；`hybrid` 可选（请求级混合检索开关：**不传 = 默认混合**；`false` = 纯向量，消融对比用；`true` = 强制混合）。两个开关均为布尔，JSON 里写 `true`/`false`，不能写字符串 `"true"`（绑定会 400）。

- **`/search`**：按 `X-User-ID` 归属过滤（admin 全库），Agent 默认入口
- **`/ask`**：**仅 `server.admin_users` 命中的管理员可调**（否则 403），**全局检索**（不按用户过滤）——它是内部测试接口，不对普通用户暴露

**指定文档对比提问**示例：先 `GET /api/v1/documents` 拿到两篇论文的 id，再限定范围提问：

```bash
curl -X POST http://localhost:8080/api/v1/ask \
  -H "Content-Type: application/json" \
  -d '{"query": "对比这两篇论文的核心观点", "document_ids": ["<doc-a-id>", "<doc-b-id>"]}'
```

### `/search` SSE 协议（2026-10 起）

`/search` 改造成 **SSE 流式**（供 Agent 转发进度到前端，让用户看到"正在检索"的实时状态）。
响应 `Content-Type: text/event-stream`，事件格式 `event: <name>` + `data: <JSON>`：

```
event: progress   data: {"stage":"embed","status":"start"}
event: progress   data: {"stage":"embed","status":"done","ms":273}
event: progress   data: {"stage":"retrieve","status":"start"}
event: progress   data: {"stage":"retrieve","status":"done","ms":411,"hits":12}
event: progress   data: {"stage":"rerank","status":"start"}        # 仅 rerank 开启且候选超池
event: progress   data: {"stage":"rerank","status":"done","ms":432}
event: result     data: {"results":[...],"total_chunks":5,"latency_ms":...}   # 完整 SearchResponse
event: error      data: {"error":"..."}                            # 检索中途失败
```

- **阶段**：`embed`（问题向量化）/ `retrieve`（三路混合粗排，`hits`=候选数）/ `rerank`（精排，仅开启时出现）。
- **行为不变**：请求体 / 归属过滤（`X-User-ID`）/ rerank 三态开关 / fail-open 全部与之前一致，只是"一次返回 JSON"变为"流式逐步返回"。
- **客户端注意**：`text/event-stream` 无 charset，SSE 里含中文（chunk 内容），读取时必须按 **UTF-8** 解码（Agent 侧已处理）。

## 黄金测试集（goldentest/）

真实论文语料的**检索+生成双轴评测体系**，思路与完整用法见 [goldentest/README.md](goldentest/README.md)。

```
goldentest/
├── README.md              # 评测体系思路 + 本地跑法（入口文档）
├── EVAL_REPORT.md         # 评测分析报告（最终基线结论，先看这个）
├── AGENT_PROMPT.md          # fact 标注 agent 提示词（quote 铁律 + nz 归一化）
├── data/                    # 入 git：纯文本数据
│   ├── docs.json            #   语料台账（12 篇真实论文/规范，含 sha256）
│   ├── facts.json           #   证据原子库（101 条 verbatim fact）
│   └── golden_queries.json  #   题目（双轴 R1-R4 × A1-A4 难度标签）
├── tools/
│   ├── g0_check.py          #   G0 数据门禁：页级（pymupdf）+ chunk 级（数据库）
│   ├── run_eval.py          #   评测 runner：调 /search + /ask，输出检索+生成指标
│   └── judge.py             #   答案正确性判定：数值程序判 + 异源 LLM 裁判（先标定后上岗）
├── corpus/                  # gitignore：PDF 语料（sha256 校验见 data/docs.json）
└── results/                 # gitignore：评测产物
```

**快速上手**：起 test 环境（模板 `config/config.test.example.yaml`）→ 设 `GOLDEN_USER_ID`/`GOLDEN_DSN` 环境变量 → `cd goldentest/tools` 跑 `g0_check.py --db`（数据门禁）与 `run_eval.py`（评测，支持 `--hybrid/--rerank` 消融开关）。完整命令与思路讲解见 [goldentest/README.md](goldentest/README.md)。


## 测试

测试用例按依赖分两类（黑盒测试，仅使用导出接口）：

| 目录 | 内容 | 依赖 |
|------|------|------|
| `tests/unit/` | 单元测试（分块/配置/余弦/PDF/HTTP 层） | 无 |
| `tests/integration/` | PostgreSQL 集成测试 | 真实数据库 |

`tests/unit/` 里的 `TestParseRealPDFs*` 是早期合成语料时代的用例（指向已迁移走的 `goldentest/pdf/` 与根级 `facts.json`），当前目录结构下自动跳过；语料级"事实逐字命中"校验已由 `goldentest/tools/g0_check.py`（页级 pymupdf + chunk 级数据库双闸）承担。

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
| `PostgresStore` | `postgres` | 生产推荐，pgvector 向量检索 + tsvector 全文检索 + pg_trgm 字面（三路 RRF 融合），数据持久化 |

接口抽象保证后续若接新数据库（Qdrant、Milvus 等）只需新增实现，业务代码零改动。

## 设计要点

- **依赖注入**：handler → service → store 接口 + ai 客户端，全部由 main 装配
- **向量检索**：pgvector `<=>` 余弦距离 + HNSW 索引，毫秒级返回
- **混合检索**：向量（语义）+ tsvector 全文（BM25 风格，gojieba 中文分词）+ pg_trgm 字面（公式/编号），三路 RRF 融合，解决公式/符号字面精确匹配弱项；三条路都遵守 `user_id` / `document_id` 归属过滤（`store.buildFilterClause` 统一生成）
- **表格解析**：框线表格识别为 Markdown；分块时表格按"表头 + N 行"独立成自包含 chunk（每个子块带表头，值不脱离列名，避免多表挤一 chunk 稀释语义）
- **公式识别**：字体指纹（Cambria Math / MT Extra / Euclid / Symbol / Times New Roman PS——纯 Times 区域需数学特征字符联合确认）检测公式区域 → **PyMuPDF 渲染 PDF 并按坐标裁剪公式截图** → 视觉模型（配置 `ai.formula.model`，当前 deepseek-ai/DeepSeek-OCR）OCR 还原 LaTeX → 公式整块成 chunk 向量化入库（公式块不与其他文本混切；截图失败降级为扁平文本）
- **引用溯源**：chunk 带页码，回答带 `[n]` 编号引用
- **状态追踪**：文档 `pending / processing / ready / failed` 四态，失败保留原因（异步摄入队列见下）
- **嵌入模型一致性**：文档与提问必须用同一 embedding 模型（同坐标系）；chat 模型可任意替换

## 多租户演进设计（归属 + 隔离，✅ 已落地）

> 记录"单用户 → 多租户"的演进方案。BYOK 已取代，**归属 + 隔离部分已实现（2026-10）**。
>
> **⚠️ 2026-10 架构演进更新（/search-only）**：本节"每个用户自带 LLM 配置（BYOK）"的前提已不成立——
> Agent 成为唯一调用方、只调 `/search`（不调 `/ask`）后，RAG 的 chat 模型上浮 Agent，RAG 只剩基础设施模型
> （embedding bge-m3 / 公式 OCR deepseek-ai/DeepSeek-OCR / rerank bge-reranker），且 **embedding 被向量维度一致性强制全局**
> （`chunks.embedding VECTOR(1024)` 必须与模型一致，无法按用户）。因此 **待做 ① `user_settings` 已被取代，不再实现**；
> RAG 侧多租户只剩 **归属 + 隔离**（`user_id` 列 + `X-User-ID` 中间件 + 去重粒度 `(user_id, content_hash)`），不涉及新表。
> 用户表只在 Agent 仓（users / threads），RAG 靠 user_id 契约关联（详见 Agent 仓 architecture.md §10）。

**多用户归属 + 隔离（✅ 已实现 2026-10）**：

| 项 | 说明 |
|----|------|
| `X-User-ID` 中间件 | `UserContext`：缺失 401；`c.Set("user_id")` 注入（`internal/httpapi/middleware.go`） |
| admin 超级用户 | `config.yaml server.admin_users`（Agent 的 user_id，通常是 UUID）：search/文档 CRUD 不过滤（全库）；ask 仅其可调 |
| 归属过滤 | search（`SearchOptions.UserID`）、文档 Get/List/Delete/Rechunk 全部按 user_id（store 层 `($2='' OR user_id=$2)`） |
| 去重按用户 | `UNIQUE(user_id, content_hash)`（`db/init.sql`） |
| body 的 user_id 移除 | `model.Question.UserID` 已删，只信 `X-User-ID` 头（修复"伪隔离"） |
| ask 仅管理员 | 全局检索的内部测试接口，非 admin → 403 |
| 历史数据迁移 | `local` → `admin`（`db/init.sql` UPDATE，幂等） |

**背景**：当前单用户模式——`documents.user_id` 固定 `"local"`，模型配置全局一份（config.yaml），分块参数已支持文档级覆盖（上传/重分块请求体携带，缺省走 config.yaml 的 `rag` 默认）。

**多用户核心诉求**：
1. 每个用户自带 LLM 配置（BYOK：base_url / api_key / model），存数据库而非 config.yaml
2. 用户自己验证模型连通性，服务器不在启动时探测
3. config.yaml 只放"全局默认/兜底"：`ai` 作为未配置自己模型的用户的默认值，`rag` 作为分块/检索参数的默认值

**已完成（2026-09）**：

| 项 | 说明 |
|----|------|
| 文档级 RAG 参数 | 上传 `POST /documents`（表单 `chunk_size`/`chunk_overlap`）、重分块 `POST /documents/:id/rechunk`（JSON）支持覆盖默认分块参数；入库存 `documents.chunk_size`/`chunk_overlap` |
| 重分块接口 | `POST /api/v1/documents/:id/rechunk`：删旧 chunk → 重新解析/分块/向量化 → 写新 |
| 连接测试接口 | `POST /api/v1/connections/test`：`{kind, base_url, api_key, model}` 发一次最小请求，返回 ok/latency_ms/error；**含基础 SSRF 防护**（禁本机/内网/保留地址） |

**待做 ① 用户 LLM 配置进数据库（BYOK）—— ⛔ 已被 /search-only 架构取代，不再实现**（见顶部演进更新）
- ~~[ ] 新建 `user_settings` 表：`user_id, base_url, api_key, model, dimension, batch_size, chat_model, ...`~~
- ~~[ ] **api_key 必须加密存储**（对称加密，密钥走环境变量，禁止明文入库）~~
- ~~[ ] `ai.Client` 从"启动时建一个全局实例"改为"按用户动态获取"：`user_id → *ai.Client` 的 LRU 缓存池，避免每请求重建 http client；缓存未命中按该用户配置重建，无配置用户回退全局默认~~
- ~~[ ] 服务层/Handler 需要拿到当前用户身份（见待做 ②）才能选对 client~~
- ~~[ ] 连接测试接口改为可"测试已存配置"（不带 creds，用库里的）~~

**待做 ② 鉴权中间件（多用户的前提）**
- [ ] 登录/注册 + token（如 JWT），`/api/v1` 下加鉴权中间件
- [ ] 把 `IngestService.defaultOwnerUserID = "local"` 换成请求上下文里的真实 user_id
- [ ] 检索归属过滤（store 已按 user_id 过滤）在鉴权后即自然生效

**顺序建议**：先做 ② 鉴权（拿到真实身份）→ 再做 ① BYOK（按身份取配置）。两者都贴近上线时再动；当前单用户阶段，config.yaml 全局默认 + 连接测试接口足够用。

**相关注意**：config.yaml 仍保留 `ai`/`rag` 节作为全局默认；检索参数（`top_k` 等）是查询级，本就由提问请求体携带；SSRF 防护目前只做了基础拦截（DNS 重绑定无法完全避免），生产建议再加代理层过滤。

## Roadmap / 下一步规划

按"RAG 后端"与"未来 Agent 项目"划分职责（RAG 保持无状态能力层，对话/流式/登录上浮到 Agent）：

### P0：补齐"完整 RAG 后端"闭环
| 项 | 归属 | 说明 |
|---|---|---|
| 异步摄入队列 | RAG | 大 PDF 摄入不再阻塞请求，工具调用先返回 job id |
| ~~混合检索（BM25 + 向量）~~ | RAG | ✅ 已实现（2026-09）：向量 + tsvector 全文（gojieba 分词）+ pg_trgm 字面，三路 RRF 融合 |
| **`/search` 纯检索接口（ADR-001 双接口演进）✅ 已实现（2026-10，且已改 SSE 流式）** | RAG | 把 `/ask` 的检索阶段独立暴露（返回 chunks + 引用，**不调 LLM**），供 Agent 自拼上下文回答；`/ask` 保留。契约见 Agent 仓库 `architecture.md` |
| ~~服务级鉴权（API Key）~~ | RAG | ✅ 已实现（2026-10）：`server.api_key` + `X-API-Key` 中间件（常量时间比较，`/health` 豁免，空值=开发模式不启用）；Agent 侧 `RAG_API_KEY` 经 session 默认头携带。用户登录在 Agent/前端 |
| Docker 化部署 | RAG + Agent | RAG 作为独立服务部署 |
| 流式输出（SSE）+ 多轮对话 | Agent 项目 | RAG 保持无状态单轮，状态/流式由 Agent 持有 |

### P1：更"工业"
- ~~**多租户 BYOK**：用户 LLM 配置进库（api_key 加密存储）+ 按用户 client 缓存池 + 鉴权中间件（把 `user_id="local"` 换成真实身份）；连接测试接口已就绪。详见上文「多租户演进设计」~~ ⛔ **2026-10 已被 /search-only 架构取代，不再实现**（RAG 只剩基础设施模型且 embedding 强制全局；多租户只剩归属+隔离，见该节顶部演进更新）
- 多格式解析（DOCX/PPT/HTML）+ 扫描件 OCR
- **公式识别增强**：① 覆盖手写/扫描图片公式（当前检测依赖字体指纹，纯图片公式无法定位区域，需要视觉检测模型）；② 支持正文内联公式；③ 换更专业的公式 OCR 模型（Pix2Text / UniMERNet，当前用 deepseek-ai/DeepSeek-OCR 截图 OCR，复杂公式偶发结构错）
- **表格解析增强**：支持纯线段表格（reportlab 等用 `m/l/S` 画的表）、无框线对齐表格、合并单元格
- **断页表格识别（待修）**：当前跨页表格被拆成两个 chunk，下页部分可能丢表头、行列错位，需支持跨页合并识别
- **表格语义化表示**：把表格转"键值句"（`状态码500：错误详情为…`）进向量，缓解数字/符号检索弱项（当前 MD 表格对数字类提问命中一般）
- Rerank 重排序（当前 MRR@10≈0.81，排序有提升空间）
- **文件存储抽象（`FileStore` 接口）+ 接 MinIO/OSS/S3**：上传的原始 PDF 改为对象存储（单机用本地 `uploads/` 即可，多实例/上线时必做——与 Docker 部署、异步摄入队列配套，原文件是"归档件"，检索热路径仍走 DB）
- 可观测性（结构化日志 / Prometheus 指标 / 链路追踪）
- 限流 / 防滥用
- 黄金测试集接入 CI 门禁

### P2：进阶
- 总结（归纳）型问题的"多证据句 + 覆盖率"评测（goldentest 已留扩展位）
- 自适应分块（按语义/标题）、元数据过滤（作者/日期/标签）
- 缓存（embedding / 检索结果 / LLM 响应）
- 安全加固（提示注入防护 / 文档级授权 / 日志脱敏）
- 文档版本管理、增量更新
