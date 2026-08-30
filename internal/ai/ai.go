package ai

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/sashabaranov/go-openai"

	"paper-rag-backend/internal/config"
)

// Client 封装 Embedding 与 Chat 两个 AI 能力，底层为 OpenAI 兼容接口。
// embedding 与 chat 各持一个独立客户端，可指向不同的 base_url。
type Client struct {
	embedding *openai.Client
	chat      *openai.Client
	cfg       config.AIConfig
}

// NewClient 根据配置创建 AI 客户端。
// embedding / chat 的 base_url、api_key 未单独设置时，回退到 ai.base_url / ai.api_key。
func NewClient(cfg config.AIConfig) *Client {
	httpClient := &http.Client{Timeout: time.Duration(cfg.TimeoutSec) * time.Second}
	return &Client{
		embedding: newOpenAIClient(
			firstNonEmpty(cfg.Embedding.APIKey, cfg.APIKey),
			firstNonEmpty(cfg.Embedding.BaseURL, cfg.BaseURL),
			httpClient,
		),
		chat: newOpenAIClient(
			firstNonEmpty(cfg.Chat.APIKey, cfg.APIKey),
			firstNonEmpty(cfg.Chat.BaseURL, cfg.BaseURL),
			httpClient,
		),
		cfg: cfg,
	}
}

func newOpenAIClient(apiKey, baseURL string, httpClient *http.Client) *openai.Client {
	ocfg := openai.DefaultConfig(apiKey)
	ocfg.BaseURL = baseURL
	ocfg.HTTPClient = httpClient
	return openai.NewClientWithConfig(ocfg)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Embed 将 texts 批量向量化，返回与输入顺序一致的向量列表。
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	all := make([][]float32, 0, len(texts))
	batch := c.cfg.Embedding.BatchSize
	if batch <= 0 {
		batch = 16
	}

	for i := 0; i < len(texts); i += batch {
		end := i + batch
		if end > len(texts) {
			end = len(texts)
		}
		req := openai.EmbeddingRequest{
			Model: openai.EmbeddingModel(c.cfg.Embedding.Model),
			Input: texts[i:end],
		}
		resp, err := c.embedding.CreateEmbeddings(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("embedding 请求失败: %w", err)
		}
		for _, d := range resp.Data {
			all = append(all, d.Embedding)
		}
	}
	return all, nil
}

// Chat 发送单轮对话，返回模型回复文本。
func (c *Client) Chat(ctx context.Context, system, user string) (string, error) {
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: system},
		{Role: openai.ChatMessageRoleUser, Content: user},
	}
	resp, err := c.chat.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model:       c.cfg.Chat.Model,
		Messages:    messages,
		Temperature: c.cfg.Chat.Temperature,
		MaxTokens:   c.cfg.Chat.MaxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("chat 请求失败: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("chat 返回空结果")
	}
	return resp.Choices[0].Message.Content, nil
}
