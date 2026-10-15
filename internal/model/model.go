package model

import "time"

// Document 表示一篇上传并解析后的文档。
type Document struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"` // 归属者；单用户模式固定为 "local"，将来多用户时由登录态赋值
	Filename    string    `json:"filename"`
	Title       string    `json:"title"`
	PageCount   int       `json:"page_count"`
	SizeBytes   int64     `json:"size_bytes"`
	ChunkCount  int       `json:"chunk_count"`
	Status      string    `json:"status"` // pending / ready / failed
	Error       string    `json:"error,omitempty"`
	ContentHash string    `json:"content_hash,omitempty"` // 原始文件 SHA-256，用于上传去重
	ChunkSize   int       `json:"chunk_size"`             // 入库时实际使用的分块大小（0=老数据未知）
	ChunkOverlap int      `json:"chunk_overlap"`          // 实际使用的重叠字符数
	CreatedAt   time.Time `json:"created_at"`
}

// IngestOptions 是上传时可选覆盖的文档级 RAG 参数。
// nil 字段表示使用 config.yaml 中 rag 的全局默认值。
// 仅分块参数（chunk_size/chunk_overlap）是"文档级"的，检索参数（top_k 等）属查询级，不在此列。
type IngestOptions struct {
	ChunkSize    *int `json:"chunk_size"`
	ChunkOverlap *int `json:"chunk_overlap"`
}

// ConnectionTest 是用户手动测试 LLM 连通性的请求。
// kind 取值：embedding（发一条最小向量化请求）/ chat（发一条最小对话请求）/ formula（发一张内置 1x1 测试图给视觉模型 OCR）。
type ConnectionTest struct {
	Kind    string `json:"kind" binding:"required"`
	BaseURL string `json:"base_url" binding:"required"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model" binding:"required"`
}

// ConnectionResult 是连接测试的结果。
type ConnectionResult struct {
	OK        bool   `json:"ok"`
	Kind      string `json:"kind"`
	LatencyMS int64  `json:"latency_ms"`
	Reply     string `json:"reply,omitempty"` // chat 测试时模型的回复
	Error     string `json:"error,omitempty"`
}

// Chunk 是文档被切分后的文本片段，带向量用于检索。
type Chunk struct {
	ID         string    `json:"id"`
	DocumentID string    `json:"document_id"`
	Filename   string    `json:"filename"`
	UserID     string    `json:"user_id,omitempty"` // 归属用户，用于检索过滤
	Page       int       `json:"page"` // 来源页码，便于引用定位
	Index      int       `json:"index"`
	Content    string    `json:"content"`
	Vector     []float32 `json:"-"`
	CreatedAt  time.Time `json:"created_at"`
}

// Question 是用户提问请求。
type Question struct {
	Query       string   `json:"query" binding:"required"`
	TopK        int      `json:"top_k"`                  // 可选，覆盖默认 top_k
	DocumentIDs []string `json:"document_ids,omitempty"` // 可选，限定检索范围；为空则全库检索
	UserID      string   `json:"user_id,omitempty"`      // 归属者；为空时服务层默认 "local"
	// Rerank 可选，请求级精排开关（三态）：
	//   nil   = 跟随 ai.rerank.enabled 配置（默认行为，老调用方不传即此）
	//   true  = 强制开启（要求 ai.rerank.model 已配置，否则无从调用、回退关闭）
	//   false = 强制关闭（临时关闭做对比/调试，不动配置）
	Rerank *bool `json:"rerank,omitempty"`
}

// UploadResult 是多文件上传时单个文件的处理结果。
// 异步语义（2026-10 起）：Success = "已接收并入队"（status=pending），
// 处理进度由前端轮询 GET /documents/:id 的 status 字段获取（pending/processing/ready/failed）。
type UploadResult struct {
	Filename   string `json:"filename"`
	Success    bool   `json:"success"` // 已接收（异步处理中），非"已入库"
	Duplicate  bool   `json:"duplicate,omitempty"` // 内容重复，已跳过（区别于失败）
	DocumentID string `json:"document_id,omitempty"`
	Status     string `json:"status,omitempty"` // 接收成功时为 "pending"；重复时为已有文档状态
	PageCount  int    `json:"page_count"` // 异步语义下接收阶段未知页数，恒为 0（入库后由文档详情返回）
	SizeBytes  int64  `json:"size_bytes"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// Answer 是 RAG 的回答结果。
type Answer struct {
	Answer      string     `json:"answer"`
	Citations   []Citation `json:"citations"`
	TotalChunks int        `json:"total_chunks"`
	LatencyMS   int64      `json:"latency_ms"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Citation 是回答引用的来源片段。
type Citation struct {
	ChunkID    string  `json:"chunk_id"`
	DocumentID string  `json:"document_id"`
	Filename   string  `json:"filename"`
	Page       int     `json:"page"`
	Score      float32 `json:"score"`
	Snippet    string  `json:"snippet"`
}
