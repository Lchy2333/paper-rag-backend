package unit

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"paper-rag-backend/internal/httpapi"
)

// RequestID 中间件与 [ACCESS] 格式扩展的验证：
// 透传 / 自生成 / 回显 / run_id 落进访问日志，四个契约一次覆盖。
// 与 APIKeyAuth / UserContext 的测试同构（见 httpapi_auth_test.go）。

func newRIDTestRouter() *gin.Engine {
	h := httpapi.NewHandler(&fakeIngester{}, nil, nil, 20, nil, nil)
	return httpapi.NewRouter(h, "")
}

// 带上游 X-Request-ID / X-Load-Test：透传 rid、回显响应头、[ACCESS] 记录两个字段。
func TestRequestID_PassthroughAndEcho(t *testing.T) {
	var buf bytes.Buffer
	orig := gin.DefaultWriter
	gin.DefaultWriter = &buf
	defer func() { gin.DefaultWriter = orig }()

	r := newRIDTestRouter()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set("X-Request-ID", "rid-test-001")
	req.Header.Set("X-Load-Test", "lt-1109")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Request-ID"); got != "rid-test-001" {
		t.Fatalf("响应头 X-Request-ID = %q, want rid-test-001（应回显透传值）", got)
	}
	if !strings.Contains(buf.String(), "rid=rid-test-001 run_id=lt-1109") {
		t.Fatalf("[ACCESS] 缺少 rid/run_id 字段，实际输出: %q", buf.String())
	}
}

// 不带上游头：就地生成 12 位 hex rid，照样回显。
func TestRequestID_GeneratesWhenMissing(t *testing.T) {
	var buf bytes.Buffer
	orig := gin.DefaultWriter
	gin.DefaultWriter = &buf
	defer func() { gin.DefaultWriter = orig }()

	r := newRIDTestRouter()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	got := rec.Header().Get("X-Request-ID")
	if len(got) != 12 {
		t.Fatalf("自生成 rid = %q, want 12 位 hex（与 Agent 侧同长度）", got)
	}
	if !strings.Contains(buf.String(), "rid="+got+" run_id=-") {
		t.Fatalf("[ACCESS] 应记自生成 rid 与 run_id=-，实际输出: %q", buf.String())
	}
}
