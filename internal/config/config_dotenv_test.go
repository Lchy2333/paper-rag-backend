package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLoadDotEnv_SetsMissingVarsOnly(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := "# 注释\nDB_TEST_A=from-dotenv\nexport DB_TEST_B=from-dotenv-export\nDB_TEST_C=should-not-override\n" +
		"DB_TEST_D=42          # 行内注释应被剥离\nDB_TEST_E=\"quoted # kept\"  # 引号内的 # 保留\n"
	if err := os.WriteFile(envPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DB_TEST_C", "from-real-env")

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prevWD)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	loadDotEnv()

	for name, want := range map[string]string{
		"DB_TEST_A": "from-dotenv",
		"DB_TEST_B": "from-dotenv-export",
		"DB_TEST_C": "from-real-env",  // 真实环境变量优先，.env 不覆盖
		"DB_TEST_D": "42",             // 行内注释剥离
		"DB_TEST_E": "quoted # kept",  // 引号内的 # 保留
	} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestLoadDotEnv_MissingFileIsNoop(t *testing.T) {
	dir := t.TempDir()
	prevWD, _ := os.Getwd()
	defer os.Chdir(prevWD)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	loadDotEnv() // 不应 panic / 报错
}

func TestExpandEnv_DefaultFallback(t *testing.T) {
	os.Unsetenv("DB_TEST_UNSET")
	got := string(expandEnv([]byte("v: ${DB_TEST_UNSET:-42}")))
	if got != "v: 42" {
		t.Errorf("缺省值未生效: %q", got)
	}

	t.Setenv("DB_TEST_SET", "7")
	got = string(expandEnv([]byte("v: ${DB_TEST_SET:-42}")))
	if got != "v: 7" {
		t.Errorf("环境变量未优先: %q", got)
	}
}

func TestLoad_EnvAndDotEnvFeedPlaceholders(t *testing.T) {
	dir := t.TempDir()
	dotenv := "DB_MAX_CONNS=23\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(dotenv), 0o644); err != nil {
		t.Fatal(err)
	}
	yamlPath := filepath.Join(dir, "config.yaml")
	y := []byte("database:\n  type: postgres\n  max_conns: ${DB_MAX_CONNS:-10}\nrag:\n  ingest_workers: ${INGEST_WORKERS:-2}\n")
	if err := os.WriteFile(yamlPath, y, 0o644); err != nil {
		t.Fatal(err)
	}

	prevWD, _ := os.Getwd()
	defer os.Chdir(prevWD)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	// Load 会跑 Validate，缺 api_key/password 会报错——这里只关心池子是否解析成功，
	// 因此直接复用 Load 前两步：loadDotEnv + expandEnv + Unmarshal。
	loadDotEnv()
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(expandEnv(data), cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Database.MaxConns != 23 {
		t.Errorf("max_conns = %d, want 23（.env 供值）", cfg.Database.MaxConns)
	}
	if cfg.RAG.IngestWorkers != 2 {
		t.Errorf("ingest_workers = %d, want 2（.env 未设 → 占位符缺省值生效）", cfg.RAG.IngestWorkers)
	}
}
