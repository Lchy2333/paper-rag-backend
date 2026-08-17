package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"paper-rag-backend/internal/ai"
	"paper-rag-backend/internal/config"
	"paper-rag-backend/internal/model"
	"paper-rag-backend/internal/store"
)

// AskService 负责 RAG 问答：向量检索 → 构造上下文 → LLM 回答。
type AskService struct {
	ai    *ai.Client
	store store.Store
	cfg   config.RAGConfig
}

// NewAskService 创建问答服务。
func NewAskService(c *ai.Client, s store.Store, cfg config.RAGConfig) *AskService {
	return &AskService{
		ai:    c,
		store: s,
		cfg:   cfg,
	}
}

// Ask 对问题做向量检索，并用命中的片段生成回答。
func (s *AskService) Ask(ctx context.Context, q model.Question) (*model.Answer, error) {
	start := time.Now()
	answer := &model.Answer{CreatedAt: start}

	// 1. 问题向量化
	vectors, err := s.ai.Embed(ctx, []string{q.Query})
	if err != nil {
		return nil, err
	}

	// 2. 向量检索
	topK := s.cfg.TopK
	if q.TopK > 0 {
		topK = q.TopK
	}
	results, err := s.store.Search(ctx, vectors[0], topK, s.cfg.SimilarityThreshold)
	if err != nil {
		return nil, fmt.Errorf("检索失败: %w", err)
	}
	answer.TotalChunks = len(results)

	if len(results) == 0 {
		answer.Answer = s.cfg.NoContextReply
		answer.LatencyMS = time.Since(start).Milliseconds()
		return answer, nil
	}

	// 3. 构造引用与上下文
	var sb strings.Builder
	for i, r := range results {
		content := strings.TrimSpace(r.Chunk.Content)
		ref := fmt.Sprintf("[%d] (来源: %s, 第 %d 页)\n%s", i+1, r.Chunk.Filename, r.Chunk.Page, content)
		sb.WriteString(ref)
		sb.WriteString("\n\n")

		answer.Citations = append(answer.Citations, model.Citation{
			ChunkID:    r.Chunk.ID,
			DocumentID: r.Chunk.DocumentID,
			Filename:   r.Chunk.Filename,
			Page:       r.Chunk.Page,
			Score:      r.Score,
			Snippet:    truncate(content, 200),
		})
	}

	// 4. LLM 生成回答
	reply, err := s.ai.Chat(ctx, s.systemPrompt(), s.userPrompt(q.Query, sb.String()))
	if err != nil {
		return nil, err
	}
	answer.Answer = strings.TrimSpace(reply)
	answer.LatencyMS = time.Since(start).Milliseconds()
	return answer, nil
}

func (s *AskService) systemPrompt() string {
	return `你是一个基于论文资料库的问答助手。请严格依据给定的资料片段回答问题。
			要求：
			1. 只使用资料片段中的信息作答，不要编造内容。
			2. 回答时在对应句末标注来源编号，例如 [1]、[2]，编号对应资料片段的序号。
			3. 如果资料片段不足以回答，请明确说明"资料中没有相关信息"。
			4. 使用与提问相同的语言回答，保持简洁准确。`
}

func (s *AskService) userPrompt(query, context string) string {
	return fmt.Sprintf("资料片段:\n\n%s\n\n问题: %s", context, query)
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}
