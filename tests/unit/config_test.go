package unit

import (
	"os"
	"path/filepath"
	"testing"

	"paper-rag-backend/internal/config"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	return p
}

func TestLoad_WithEnvExpansion(t *testing.T) {
	os.Setenv("TEST_EMBED_KEY", "sk-env-test")
	defer os.Unsetenv("TEST_EMBED_KEY")

	p := writeTemp(t, `
server:
  port: 9090
ai:
  base_url: "https://example.com/v1"
  api_key: "${TEST_EMBED_KEY}"
  embedding:
    model: "text-embedding-3-small"
    dimension: 1536
  chat:
    model: "gpt-4o-mini"
rag:
  chunk_size: 500
  chunk_overlap: 100
  top_k: 3
database:
  type: "postgres"
  password: "db-pass"
`)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.Server.Port != 9090 {
		t.Fatalf("port = %d, want 9090", cfg.Server.Port)
	}
	if cfg.AI.APIKey != "sk-env-test" {
		t.Fatalf("api_key = %q, want env 展开后的 sk-env-test", cfg.AI.APIKey)
	}
	if cfg.RAG.TopK != 3 {
		t.Fatalf("top_k = %d, want 3", cfg.RAG.TopK)
	}
}

func TestLoad_EnvDefaultFallback(t *testing.T) {
	p := writeTemp(t, `
ai:
  base_url: "https://example.com/v1"
  api_key: "${NOT_SET_KEY:-default-fallback}"
  embedding:
    model: "m"
  chat:
    model: "c"
database:
  type: "postgres"
  password: "db-pass"
`)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.AI.APIKey != "default-fallback" {
		t.Fatalf("api_key = %q, want default-fallback", cfg.AI.APIKey)
	}
}

func TestLoad_MissingAPIKey(t *testing.T) {
	p := writeTemp(t, `
ai:
  base_url: "https://example.com/v1"
  embedding:
    model: "m"
  chat:
    model: "c"
`)
	if _, err := config.Load(p); err == nil {
		t.Fatalf("缺少 api_key 时应报错")
	}
}
