package unit

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"paper-rag-backend/internal/httpapi"
)

// newAuthTestRouter 构造带指定 API Key 的路由，用 GET /api/v1/documents 作为受保护接口代表。
func newAuthTestRouter(apiKey string) *gin.Engine {
	h := httpapi.NewHandler(&fakeIngester{}, nil, nil, nil, 20, nil, nil)
	return httpapi.NewRouter(h, apiKey)
}

func doRequest(r *gin.Engine, method, path, apiKey, userID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	if userID != "" {
		req.Header.Set("X-User-ID", userID)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// 未配置 key（空）= 开发模式：不带 key 的请求照常放行（仍需 X-User-ID）。
func TestAPIKeyAuth_DisabledWhenEmpty(t *testing.T) {
	r := newAuthTestRouter("")
	rec := doRequest(r, http.MethodGet, "/api/v1/documents", "", "admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（空 key 应放行）", rec.Code)
	}
}

// 配置了 key：缺失 X-API-Key → 401。
func TestAPIKeyAuth_RejectsMissingKey(t *testing.T) {
	r := newAuthTestRouter("secret-key")
	rec := doRequest(r, http.MethodGet, "/api/v1/documents", "", "admin")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401（缺 key 应拒绝）", rec.Code)
	}
}

// 配置了 key：错误的 key → 401。
func TestAPIKeyAuth_RejectsWrongKey(t *testing.T) {
	r := newAuthTestRouter("secret-key")
	rec := doRequest(r, http.MethodGet, "/api/v1/documents", "wrong-key", "admin")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401（错 key 应拒绝）", rec.Code)
	}
}

// 配置了 key：正确的 key → 200 放行进入业务 handler。
func TestAPIKeyAuth_AcceptsCorrectKey(t *testing.T) {
	r := newAuthTestRouter("secret-key")
	rec := doRequest(r, http.MethodGet, "/api/v1/documents", "secret-key", "admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（对 key 应放行）", rec.Code)
	}
}

// /health 不鉴权：即使配置了 key，不带 key 也应 200（供 compose healthcheck 使用）。
func TestAPIKeyAuth_HealthBypassesAuth(t *testing.T) {
	r := newAuthTestRouter("secret-key")
	rec := doRequest(r, http.MethodGet, "/api/v1/health", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（/health 应豁免鉴权）", rec.Code)
	}
}

// 鉴权对所有受保护方法生效（DELETE 也要拦）。
func TestAPIKeyAuth_ProtectsDelete(t *testing.T) {
	r := newAuthTestRouter("secret-key")
	rec := doRequest(r, http.MethodDelete, "/api/v1/documents/abc", "", "admin")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401（DELETE 缺 key 应拒绝）", rec.Code)
	}
}

// ---- UserContext：X-User-ID 缺失 → 401（多用户后不允许无归属请求） ----

func TestUserContext_RejectsMissingUserID(t *testing.T) {
	r := newAuthTestRouter("")
	rec := doRequest(r, http.MethodGet, "/api/v1/documents", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401（缺 X-User-ID 应拒绝）", rec.Code)
	}
}

func TestUserContext_AcceptsWithUserID(t *testing.T) {
	r := newAuthTestRouter("")
	rec := doRequest(r, http.MethodGet, "/api/v1/documents", "", "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（带 X-User-ID 应放行）", rec.Code)
	}
}
