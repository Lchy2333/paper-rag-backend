package ai

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"

	"paper-rag-backend/internal/config"
)

// Client 封装 Embedding、Chat 与公式 OCR 三个 AI 能力，底层为 OpenAI 兼容接口。
// 三个能力各持一个独立客户端，可指向不同的 base_url。
type Client struct {
	embedding *openai.Client
	chat      *openai.Client
	formula   *openai.Client
	cfg       config.AIConfig
}

// NewClient 根据配置创建 AI 客户端。
// 各能力的 base_url、api_key 未单独设置时，回退到 ai.base_url / ai.api_key。
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
		formula: newOpenAIClient(
			firstNonEmpty(cfg.Formula.APIKey, cfg.APIKey),
			firstNonEmpty(cfg.Formula.BaseURL, cfg.BaseURL),
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

// formulaSystemPrompt 引导多模态模型只输出公式的 LaTeX 代码，不带任何多余文字。
const formulaSystemPrompt = `你是公式识别引擎。识别图片中的数学公式，只输出 LaTeX 代码本身：
- 不要任何解释、前言或结语
- 不要代码块标记（不要用三个反引号包裹）
- 只输出一个 LaTeX 公式，不要编号`

// FormulaOCR 将公式图片识别为 LaTeX。
// imageBytes 为公式裁剪图（PNG/JPG 等）；模型用 ai.formula 配置的多模态模型。
// 返回模型给出的 LaTeX 源码；模型未配置时返回错误。
func (c *Client) FormulaOCR(ctx context.Context, imageBytes []byte) (string, error) {
	if c.cfg.Formula.Model == "" {
		return "", fmt.Errorf("公式 OCR 未配置：请设置 ai.formula.model（如 qwen2.5vl:7b）")
	}
	if len(imageBytes) == 0 {
		return "", fmt.Errorf("公式图片为空")
	}

	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageBytes)
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: formulaSystemPrompt},
		{Role: openai.ChatMessageRoleUser, MultiContent: []openai.ChatMessagePart{
			{Type: openai.ChatMessagePartTypeImageURL, ImageURL: &openai.ChatMessageImageURL{URL: dataURL}},
		}},
	}
	resp, err := c.formula.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model:       c.cfg.Formula.Model,
		Messages:    messages,
		Temperature: c.cfg.Formula.Temperature,
		MaxTokens:   c.cfg.Formula.MaxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("公式 OCR 请求失败: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("公式 OCR 返回空结果")
	}
	return CleanLatex(resp.Choices[0].Message.Content), nil
}

// formulaTextSystemPrompt 引导模型从扁平文本还原 LaTeX：
// 扁平文本来自 PDF 文本层，可能顺序错乱、行代表上下结构（分式分子/分母、上下标）。
const formulaTextSystemPrompt = `你是公式还原引擎。下面是 PDF 文本层提取出的一个数学公式的扁平文本：
- 每个视觉行一行，行从上到下排列（分式分子在上、分母在下，上标/下标是独立小行）
- 行内字符按从左到右排列
请把它还原成正确的 LaTeX 代码，只输出 LaTeX 代码本身：
- 不要任何解释、前言或结语
- 不要代码块标记（不要用三个反引号包裹）
- 只输出一个 LaTeX 公式，不要编号`

// FormulaTextToLatex 从公式的扁平文本还原 LaTeX。
// flatText 为检测模块提取的扁平文本（行间 \n 分隔），无需图片，走纯文本接口。
// 返回还原后的 LaTeX 源码；模型未配置时返回错误。
func (c *Client) FormulaTextToLatex(ctx context.Context, flatText string) (string, error) {
	if c.cfg.Formula.Model == "" {
		return "", fmt.Errorf("公式 OCR 未配置：请设置 ai.formula.model（如 qwen2.5vl:7b）")
	}
	if strings.TrimSpace(flatText) == "" {
		return "", fmt.Errorf("公式扁平文本为空")
	}

	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: formulaTextSystemPrompt},
		{Role: openai.ChatMessageRoleUser, Content: flatText},
	}
	resp, err := c.formula.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model:       c.cfg.Formula.Model,
		Messages:    messages,
		Temperature: c.cfg.Formula.Temperature,
		MaxTokens:   c.cfg.Formula.MaxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("公式还原请求失败: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("公式还原返回空结果")
	}
	return CleanLatex(resp.Choices[0].Message.Content), nil
}

// FormulaModel 返回公式 OCR 当前配置的模型名（用于日志展示）。
func (c *Client) FormulaModel() string {
	if c.cfg.Formula.Model == "" {
		return "(未配置)"
	}
	return c.cfg.Formula.Model
}

// CleanLatex 清洗模型返回的 LaTeX，去掉常见的多余内容：
//   - 外层定界符：$$ ... $$、$ ... $、\[ ... \]、\( ... \)
//   - 代码块围栏：```latex ... ```
//   - 结尾悬空的箭头命令（如 \leftarrow），这类是模型的幻觉尾巴
func CleanLatex(s string) string {
	s = strings.TrimSpace(s)

	// 去代码块围栏
	if strings.HasPrefix(s, "```") {
		lines := strings.Split(s, "\n")
		if len(lines) > 0 {
			lines = lines[1:]
		}
		if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "```") {
			lines = lines[:len(lines)-1]
		}
		s = strings.Join(lines, "\n")
	}

	// 去外层定界符（可能多层）
	for {
		orig := s
		switch {
		case strings.HasPrefix(s, "$$") && strings.HasSuffix(s, "$$"):
			s = s[2 : len(s)-2]
		case strings.HasPrefix(s, "$") && strings.HasSuffix(s, "$"):
			s = s[1 : len(s)-1]
		case strings.HasPrefix(s, `\[`) && strings.HasSuffix(s, `\]`):
			s = s[2 : len(s)-2]
		case strings.HasPrefix(s, `\(`) && strings.HasSuffix(s, `\)`):
			s = s[2 : len(s)-2]
		}
		s = strings.TrimSpace(s)
		if s == orig {
			break
		}
	}

	// 去结尾悬空的箭头命令（模型幻觉尾巴），长的先匹配
	for _, tail := range []string{`\longrightarrow`, `\longleftarrow`, `\leftrightarrow`, `\rightarrow`, `\leftarrow`} {
		if strings.HasSuffix(s, tail) {
			s = strings.TrimRight(strings.TrimSuffix(s, tail), " ")
			break
		}
	}
	return strings.TrimSpace(s)
}
