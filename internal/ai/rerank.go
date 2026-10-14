// rerank.go 实现 cross-encoder 精排客户端（/rerank 协议，Jina/Cohere 形状）。
//
// 与 Embedding/Chat 不同，rerank 不是 OpenAI 兼容协议（go-openai 库没有 rerank），
// 因此手写 HTTP。协议形状（SiliconFlow / Xinference 均同款）：
//
//	POST {base_url}/rerank
//	Authorization: Bearer <key>
//	{ "model": "...", "query": "...", "documents": ["..."], "top_n": 5, "return_documents": false }
//	→ { "results": [ { "index": 0, "relevance_score": 0.93 }, ... ] }
//
// index 是传入 documents 数组的下标，API 不回传文档内容（return_documents: false
// 时 document 为 null），调用方按下标回查自己的候选数组。
// 实测（2026-10，SiliconFlow）：token 用量在 meta.tokens 字段，没有 usage 字段。
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"paper-rag-backend/internal/logger"
)

// rerankRequest 是 /rerank 请求体。
type rerankRequest struct {
	Model           string   `json:"model"`
	Query           string   `json:"query"`
	Documents       []string `json:"documents"`
	TopN            int      `json:"top_n,omitempty"`
	ReturnDocuments bool     `json:"return_documents"`
}

// rerankItem 是单个候选的重排结果：index 为传入 documents 的下标。
type rerankItem struct {
	Index          int     `json:"index"`
	RelevanceScore float32 `json:"relevance_score"`
}

// rerankResponse 是 /rerank 响应（只取 results，id/meta.tokens 略）。
type rerankResponse struct {
	Results []rerankItem `json:"results"`
}

// RerankResult 是暴露给服务层的重排结果。
type RerankResult struct {
	Index int     // 在传入 documents 中的原始下标
	Score float32 // cross-encoder 相关性分数（0~1，量纲与余弦相似度不同）
}

// Rerank 用 cross-encoder 对 (query, documents) 逐对打分，返回按分数降序的结果。
// 返回的 Index 用于调用方回查自己的候选数组（API 不回传文档内容）。
func (c *Client) Rerank(ctx context.Context, query string, documents []string, topN int) ([]RerankResult, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	// 卫语句只管"有没有模型可调"（configured）；"要不要调"（enabled/请求级开关）
	// 是 service 层的策略决定，这里不越权拦截。
	if !c.RerankConfigured() {
		return nil, fmt.Errorf("rerank 未配置：请设置 ai.rerank.model")
	}

	body, err := json.Marshal(rerankRequest{
		Model:           c.cfg.Rerank.Model,
		Query:           query,
		Documents:       documents,
		TopN:            topN,
		ReturnDocuments: false, // 只要下标+分数，省流量
	})
	if err != nil {
		return nil, fmt.Errorf("rerank 请求序列化失败: %w", err)
	}

	url := strings.TrimRight(c.rerankBaseURL(), "/") + "/rerank"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("rerank 构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// 云 API 必须带 Bearer 鉴权（与本地 Ollama 的占位 key 不同）
	req.Header.Set("Authorization", "Bearer "+c.rerankAPIKey())

	start := time.Now()
	resp, err := c.rerankHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rerank 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("rerank 响应异常: HTTP %d: %s", resp.StatusCode, snippet)
	}

	var rr rerankResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return nil, fmt.Errorf("rerank 响应解析失败: %w", err)
	}

	out := make([]RerankResult, 0, len(rr.Results))
	for _, it := range rr.Results {
		if it.Index < 0 || it.Index >= len(documents) {
			continue // 防御：API 返回越界下标时跳过，不 panic
		}
		out = append(out, RerankResult{Index: it.Index, Score: it.RelevanceScore})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })

	logger.Debug("rerank.done", "docs", len(documents), "hits", len(out),
		"duration_ms", time.Since(start).Milliseconds())
	return out, nil
}

// RerankEnabled 报告白排是否可用（服务层据此决定是否扩池/精排）。
func (c *Client) RerankEnabled() bool {
	return c.cfg.Rerank.Enabled && c.cfg.Rerank.Model != ""
}

// RerankConfigured 报告 rerank 模型是否已配置（不管 enabled 开关）。
// 请求级强制开启（question.rerank = true）的前提：得有模型可调。
func (c *Client) RerankConfigured() bool {
	return c.cfg.Rerank.Model != ""
}

// RerankPool 返回粗排候选池大小（服务层据此放宽检索 LIMIT）。
func (c *Client) RerankPool() int {
	if c.cfg.Rerank.CandidatePool > 0 {
		return c.cfg.Rerank.CandidatePool
	}
	return 30
}

// RerankTopN 返回精排后保留数（0 = 沿用 rag.top_k / 请求级 top_k）。
func (c *Client) RerankTopN() int {
	return c.cfg.Rerank.TopN
}

func (c *Client) rerankBaseURL() string {
	return firstNonEmpty(c.cfg.Rerank.BaseURL, c.cfg.BaseURL)
}

func (c *Client) rerankAPIKey() string {
	return firstNonEmpty(c.cfg.Rerank.APIKey, c.cfg.APIKey)
}
