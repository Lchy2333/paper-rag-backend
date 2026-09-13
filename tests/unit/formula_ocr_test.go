package unit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"paper-rag-backend/internal/ai"
	"paper-rag-backend/internal/config"
)

// mockFormulaServer 模拟 Ollama 的 OpenAI 兼容 /chat/completions 接口，
// 校验请求里带了公式图片（data URL）并返回固定 LaTeX。
func mockFormulaServer(t *testing.T, latex string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s, want /chat/completions", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("解析请求失败: %v", err)
		}
		if req.Model != "qwen2.5vl:7b" {
			t.Errorf("model = %q, want qwen2.5vl:7b", req.Model)
		}
		if len(req.Messages) != 2 {
			t.Errorf("messages 数 = %d, want 2", len(req.Messages))
		}
		var parts []struct {
			Type     string `json:"type"`
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if err := json.Unmarshal(req.Messages[1].Content, &parts); err != nil {
			t.Errorf("解析 content parts 失败: %v", err)
		}
		hasImage := false
		for _, part := range parts {
			if part.Type == "image_url" && part.ImageURL != nil && strings.HasPrefix(part.ImageURL.URL, "data:image/png;base64,") {
				hasImage = true
			}
		}
		if !hasImage {
			t.Errorf("user 消息里没有 data:image/png;base64 图片")
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","created":0,"model":"qwen2.5vl:7b","choices":[{"index":0,"message":{"role":"assistant","content":` + mustJSON(t, latex) + `},"finish_reason":"stop"}]}`))
	}))
	return srv
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal %q 失败: %v", s, err)
	}
	return string(b)
}

func TestFormulaOCR_Success(t *testing.T) {
	srv := mockFormulaServer(t, `\frac{c}{a}=b`)
	defer srv.Close()

	client := ai.NewClient(config.AIConfig{
		BaseURL: srv.URL,
		APIKey:  "test",
		Formula: config.FormulaConfig{Model: "qwen2.5vl:7b", MaxTokens: 1024},
	})

	got, err := client.FormulaOCR(t.Context(), []byte("fake-png-bytes"))
	if err != nil {
		t.Fatalf("FormulaOCR 失败: %v", err)
	}
	if got != `\frac{c}{a}=b` {
		t.Fatalf("latex = %q, want \\frac{c}{a}=b", got)
	}
}

func TestFormulaOCR_NoModelConfigured(t *testing.T) {
	client := ai.NewClient(config.AIConfig{BaseURL: "http://localhost:1", APIKey: "test"})
	if _, err := client.FormulaOCR(t.Context(), []byte("x")); err == nil {
		t.Fatalf("未配置 formula.model 时应报错")
	}
}

func TestFormulaOCR_EmptyImage(t *testing.T) {
	srv := mockFormulaServer(t, "")
	defer srv.Close()
	client := ai.NewClient(config.AIConfig{
		BaseURL: srv.URL,
		APIKey:  "test",
		Formula: config.FormulaConfig{Model: "qwen2.5vl:7b"},
	})
	if _, err := client.FormulaOCR(t.Context(), nil); err == nil {
		t.Fatalf("空图片时应报错")
	}
}

func TestCleanLatex(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`$$\int_{0}^{1} a = \sum_{3} b + c \leftarrow$$`, `\int_{0}^{1} a = \sum_{3} b + c`},
		{`\frac{c}{a}=b`, `\frac{c}{a}=b`},
		{"```latex\n\\sqrt{2}\n```", `\sqrt{2}`},
		{`$a=b$`, `a=b`},
		{`\[ \sum_{i=1}^{n} i \]`, `\sum_{i=1}^{n} i`},
		{`$$x$$`, `x`},
	}
	for _, c := range cases {
		if got := ai.CleanLatex(c.in); got != c.want {
			t.Fatalf("CleanLatex(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
