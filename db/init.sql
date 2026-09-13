-- paper_rag 数据库初始化脚本
-- 用法: psql -U postgres -f db/init.sql
-- 或在 Navicat 中新建数据库 paper_rag 后，在查询窗口执行本文件内容。

-- 1. 开启向量扩展（每个库执行一次）
CREATE EXTENSION IF NOT EXISTS vector;

-- 2. 文档表：每篇论文一行
-- user_id: 文档归属者。当前单用户模式固定为 'local'，为将来多用户隔离铺路；
--          接入鉴权后应由登录态动态赋值。
CREATE TABLE IF NOT EXISTS documents (
    id           VARCHAR(32) PRIMARY KEY,
    user_id      VARCHAR(64) NOT NULL DEFAULT 'local',
    filename     VARCHAR(255) NOT NULL,
    title        VARCHAR(255),
    page_count   INTEGER NOT NULL DEFAULT 0,
    size_bytes   BIGINT  NOT NULL DEFAULT 0,
    chunk_count  INTEGER NOT NULL DEFAULT 0,
    status       VARCHAR(20) NOT NULL DEFAULT 'pending',
    error        TEXT,
    content_hash VARCHAR(64),
    -- 文档级分块参数：入库时实际使用的值（0 = 老数据/未记录）。config.yaml 的 rag 仅作默认值。
    chunk_size   INTEGER NOT NULL DEFAULT 0,
    chunk_overlap INTEGER NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 2.2 老库迁移：已存在的 documents 表补分块参数列（幂等，可重复执行）
ALTER TABLE documents ADD COLUMN IF NOT EXISTS chunk_size   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE documents ADD COLUMN IF NOT EXISTS chunk_overlap INTEGER NOT NULL DEFAULT 0;

-- 2.1 内容哈希去重：同一文件 SHA-256 唯一（failed 文档除外，允许重传重试）
CREATE UNIQUE INDEX IF NOT EXISTS idx_documents_content_hash
    ON documents(content_hash) WHERE status <> 'failed';

-- 3. 分块向量表：每个文本片段一行，含向量
--    注意: embedding 维度必须与 config.yaml 中 ai.embedding.dimension 一致
--    默认使用 Ollama bge-m3 (1024 维)
CREATE TABLE IF NOT EXISTS chunks (
    id          VARCHAR(32) PRIMARY KEY,
    document_id VARCHAR(32) NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    page        INTEGER NOT NULL,
    idx         INTEGER NOT NULL,
    content     TEXT NOT NULL,
    embedding   VECTOR(1024),
    -- 中文分词 tsvector：Go 侧 gojieba 分词后经 to_tsvector('simple') 生成，
    -- 供 BM25 风格全文检索（绕开 PostgreSQL 无中文分词器的问题）
    tsv         tsvector,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 4. 向量检索索引（HNSW + 余弦相似度）
--    注意: 若 embedding 模型维度不是 1536，请先调整 chunks.embedding 的维度
CREATE INDEX IF NOT EXISTS idx_chunks_embedding ON chunks USING hnsw (embedding vector_cosine_ops);

-- 4.1 文档过滤索引：两阶段检索的"补漏"阶段按 document_id 走 B-tree 精确取回，
--     避免 HNSW 粗筛在过滤条件下扫不到足够候选。也加速文档删除的级联清理。
CREATE INDEX IF NOT EXISTS idx_chunks_document_id ON chunks(document_id);

-- 4.2 全文检索索引：tsvector GIN（BM25 风格全文检索）
CREATE INDEX IF NOT EXISTS idx_chunks_tsv ON chunks USING gin (tsv);
