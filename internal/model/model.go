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
}

// UploadResult 是多文件上传时单个文件的处理结果。
type UploadResult struct {
	Filename   string `json:"filename"`
	Success    bool   `json:"success"`
	Duplicate  bool   `json:"duplicate,omitempty"` // 内容重复，已跳过（区别于失败）
	DocumentID string `json:"document_id,omitempty"`
	PageCount  int    `json:"page_count"`
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
