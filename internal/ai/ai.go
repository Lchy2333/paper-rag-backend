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
type Client struct {
	openai *openai.Client
	cfg    config.AIConfig
}

// NewClient 根据配置创建 AI 客户端。
func NewClient(cfg config.AIConfig) *Client {
	ocfg := openai.DefaultConfig(cfg.APIKey)
	ocfg.BaseURL = cfg.BaseURL
	ocfg.HTTPClient = &http.Client{Timeout: time.Duration(cfg.TimeoutSec) * time.Second}
	return &Client{
		openai: openai.NewClientWithConfig(ocfg),
		cfg:    cfg,
	}
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
		resp, err := c.openai.CreateEmbeddings(ctx, req)
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
	resp, err := c.openai.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
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
