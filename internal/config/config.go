package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config 是服务根配置，对应 config.yaml 文件结构。
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	AI       AIConfig       `yaml:"ai"`
	RAG      RAGConfig      `yaml:"rag"`
	Database DatabaseConfig `yaml:"database"`
	Render   RenderConfig   `yaml:"render"`
}

type ServerConfig struct {
	Host            string `yaml:"host"`
	Port            int    `yaml:"port"`
	MaxUploadSizeMB int64  `yaml:"max_upload_size_mb"`
	UploadDir       string `yaml:"upload_dir"`
	ReadTimeoutSec  int    `yaml:"read_timeout_sec"`
	WriteTimeoutSec int    `yaml:"write_timeout_sec"`
}

type AIConfig struct {
	Provider   string          `yaml:"provider"`
	BaseURL    string          `yaml:"base_url"`
	APIKey     string          `yaml:"api_key"`
	TimeoutSec int             `yaml:"timeout_sec"`
	Embedding  EmbeddingConfig `yaml:"embedding"`
	Chat       ChatConfig      `yaml:"chat"`
	Formula    FormulaConfig   `yaml:"formula"`
}

type EmbeddingConfig struct {
	Model     string `yaml:"model"`
	Dimension int    `yaml:"dimension"`
	BatchSize int    `yaml:"batch_size"`
	BaseURL   string `yaml:"base_url"` // 可选，为空则回退到 ai.base_url
	APIKey    string `yaml:"api_key"`  // 可选，为空则回退到 ai.api_key
}

type ChatConfig struct {
	Model       string  `yaml:"model"`
	Temperature float32 `yaml:"temperature"`
	MaxTokens   int     `yaml:"max_tokens"`
	BaseURL     string  `yaml:"base_url"` // 可选，为空则回退到 ai.base_url
	APIKey      string  `yaml:"api_key"`  // 可选，为空则回退到 ai.api_key
}

// FormulaConfig 是公式 OCR（图片 → LaTeX）配置。
// 使用多模态模型（如 Ollama 的 qwen2.5vl:7b），base_url / api_key 为空时回退到 ai.base_url / ai.api_key。
type FormulaConfig struct {
	Model       string  `yaml:"model"`
	Temperature float32 `yaml:"temperature"`
	MaxTokens   int     `yaml:"max_tokens"`
	BaseURL     string  `yaml:"base_url"` // 可选，为空则回退到 ai.base_url
	APIKey      string  `yaml:"api_key"`  // 可选，为空则回退到 ai.api_key
}

type RAGConfig struct {
	ChunkSize           int     `yaml:"chunk_size"`
	ChunkOverlap        int     `yaml:"chunk_overlap"`
	TopK                int     `yaml:"top_k"`
	SimilarityThreshold float32 `yaml:"similarity_threshold"`
	NoContextReply      string  `yaml:"no_context_reply"`
}

// DatabaseConfig 是 PostgreSQL 连接配置。
type DatabaseConfig struct {
	// Type 存储实现类型，当前仅支持 postgres（本地数据库）
	Type     string `yaml:"type"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
	SSLMode  string `yaml:"sslmode"`
	// MaxConns 连接池最大连接数，0 表示使用 pgx 默认值
	MaxConns int `yaml:"max_conns"`
}

// RenderConfig 是 PDF 渲染（公式区域截图）配置。
// 渲染用 PyMuPDF（pip install pymupdf），底层是 MuPDF 内核，
// 公式 OCR 需要把 PDF 页面渲染成图片后裁剪出公式区域。
type RenderConfig struct {
	// PythonCmd Python 可执行文件；为空默认 "python"。
	PythonCmd string `yaml:"python_cmd"`
	// DPI 渲染分辨率，公式截图建议 150（太小字糊、太大慢）
	DPI int `yaml:"dpi"`
	// WorkDir 渲染临时文件目录，为空用系统临时目录
	WorkDir string `yaml:"work_dir"`
}

// DSN 生成 pgx 连接字符串（键值对格式，密码含特殊字符也能安全处理）。
func (d DatabaseConfig) DSN() string {
	if d.SSLMode == "" {
		d.SSLMode = "disable"
	}
	if d.Port == 0 {
		d.Port = 5432
	}
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		d.Host, d.Port, d.User, pgQuote(d.Password), d.DBName, d.SSLMode)
}

// pgQuote 对连接串里的值做安全引用：值含空格/单引号/反斜杠时用单引号包裹。
func pgQuote(s string) string {
	if !strings.ContainsAny(s, " '\\") {
		return s
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `''`)
	return "'" + s + "'"
}

// Load 从 path 读取 YAML 配置并解析环境变量占位符。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	data = expandEnv(data)

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	cfg.applyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// expandEnv 将 ${VAR} 或 ${VAR:-default} 替换为环境变量值。
func expandEnv(data []byte) []byte {
	return envPattern.ReplaceAllFunc(data, func(m []byte) []byte {
		sub := envPattern.FindStringSubmatch(string(m))
		name, def := sub[1], sub[2]
		if v, ok := os.LookupEnv(name); ok {
			return []byte(v)
		}
		if def != "" {
			return []byte(def)
		}
		return m
	})
}

func (c *Config) applyDefaults() {
	if c.Server.Port == 0 {
		c.Server.Port = 8080
	}
	if c.Server.MaxUploadSizeMB == 0 {
		c.Server.MaxUploadSizeMB = 20
	}
	if c.Server.UploadDir == "" {
		c.Server.UploadDir = "./uploads"
	}
	if c.Server.Host == "" {
		c.Server.Host = "0.0.0.0"
	}
	if c.Database.Type == "" {
		c.Database.Type = "postgres"
	}
	if c.Database.SSLMode == "" {
		c.Database.SSLMode = "disable"
	}
	if c.AI.TimeoutSec == 0 {
		c.AI.TimeoutSec = 120
	}
	if c.AI.Embedding.BatchSize == 0 {
		c.AI.Embedding.BatchSize = 16
	}
	if c.AI.Chat.MaxTokens == 0 {
		c.AI.Chat.MaxTokens = 2048
	}
	if c.AI.Formula.MaxTokens == 0 {
		c.AI.Formula.MaxTokens = 1024
	}
	if c.RAG.ChunkSize == 0 {
		c.RAG.ChunkSize = 800
	}
	if c.RAG.TopK == 0 {
		c.RAG.TopK = 5
	}
	if c.Render.DPI == 0 {
		c.Render.DPI = 150
	}
}

// Validate 检查关键配置是否合法。
func (c *Config) Validate() error {
	if c.AI.BaseURL == "" && c.AI.Embedding.BaseURL == "" {
		return fmt.Errorf("ai.base_url（或 ai.embedding.base_url）不能为空，请参考 config/config.yaml 中的注释")
	}
	if c.AI.BaseURL == "" && c.AI.Chat.BaseURL == "" {
		return fmt.Errorf("ai.base_url（或 ai.chat.base_url）不能为空，请参考 config/config.yaml 中的注释")
	}
	if c.AI.APIKey == "" && c.AI.Embedding.APIKey == "" && c.AI.Chat.APIKey == "" {
		return fmt.Errorf("ai.api_key（或 ai.embedding.api_key / ai.chat.api_key）不能为空（可通过环境变量注入，如 ${OPENAI_API_KEY}）")
	}
	if c.AI.Embedding.Model == "" {
		return fmt.Errorf("ai.embedding.model 不能为空")
	}
	if c.AI.Chat.Model == "" {
		return fmt.Errorf("ai.chat.model 不能为空")
	}
	if c.Database.Type == "postgres" && c.Database.Password == "" {
		return fmt.Errorf("database.password 不能为空（可通过环境变量注入，如 ${POSTGRES_PASSWORD}）")
	}
	if c.RAG.ChunkOverlap >= c.RAG.ChunkSize {
		return fmt.Errorf("rag.chunk_overlap (%d) 必须小于 rag.chunk_size (%d)", c.RAG.ChunkOverlap, c.RAG.ChunkSize)
	}
	return nil
}

func (c *Config) Addr() string {
	return c.Server.Host + ":" + strconv.Itoa(c.Server.Port)
}
