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
uploads/               # 上传的 PDF 落盘目录
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
  base_url: "http://localhost:11434/v1"   # Ollama；换云端服务则改成对应地址
  api_key: "ollama"                       # Ollama 不需要 key，填任意值
  embedding:
    model: "bge-m3"
    dimension: 1024                       # ⚠️ 必须与数据库 VECTOR(n) 一致
  chat:
    model: "qwen3:8b"
database:
  type: "postgres"                        # postgres | memory
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

`database.type` 设为 `memory` 可跳过数据库（数据仅存内存，重启丢失）。

### 4. 启动服务

```bash
go run ./cmd/server -config config/config.yaml
```

默认监听 `0.0.0.0:8080`，启动后访问 `GET /api/v1/health` 验证。

### 5. 使用

**上传 PDF 并入库**

```bash
curl -X POST http://localhost:8080/api/v1/documents -F "file=@paper.pdf"
```

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
| POST | `/api/v1/documents` | 上传 PDF（multipart 字段 `file`），解析并向量化入库 |
| GET | `/api/v1/documents` | 列出全部文档 |
| GET | `/api/v1/documents/:id` | 查询文档详情 |
| DELETE | `/api/v1/documents/:id` | 删除文档及向量（级联删除 chunks）|
| POST | `/api/v1/ask` | 提问，返回基于检索片段的回答与引用 |

`/ask` 请求体：

```json
{
    "query": "问题内容",
    "top_k": 5
}
```

`query` 必填；`top_k` 可选（检索片段数，默认取配置 `rag.top_k`）。

## 测试

```bash
go test ./...            # 单元测试（不含数据库集成测试）
```

PostgreSQL 集成测试需提供真实数据库密码（未设置则自动跳过）：

```powershell
$env:TEST_DATABASE_PASSWORD = "你的数据库密码"
go test ./internal/store -run TestPostgresStore -v
```

## 存储层切换

`internal/store` 定义了统一 `Store` 接口，当前有两个实现：

| 实现 | 配置 `database.type` | 特点 |
|------|---------------------|------|
| `PostgresStore` | `postgres` | 生产推荐，pgvector 向量检索，数据持久化 |
| `MemoryStore` | `memory` | 开发/测试用，重启丢失 |

切换只改配置，业务代码零改动。

## 设计要点

- **依赖注入**：handler → service → store 接口 + ai 客户端，全部由 main 装配
- **向量检索**：pgvector `<=>` 余弦距离 + HNSW 索引，毫秒级返回
- **引用溯源**：chunk 带页码，回答带 `[n]` 编号引用
- **状态追踪**：文档 `pending / ready / failed` 三态，失败保留原因
- **嵌入模型一致性**：文档与提问必须用同一 embedding 模型（同坐标系）；chat 模型可任意替换
