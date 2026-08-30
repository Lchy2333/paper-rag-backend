package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"paper-rag-backend/internal/httpapi"
	"paper-rag-backend/internal/model"
	"paper-rag-backend/internal/service"
)

// fakeIngester 是 documentService 的假实现，用于测试 HTTP 层。
type fakeIngester struct {
	errs map[string]error
	dups map[string]error
}

func (f *fakeIngester) Process(_ context.Context, _ io.ReaderAt, size int64, filename string) (*model.Document, error) {
	if f.dups != nil {
		if _, ok := f.dups[filename]; ok {
			// 模拟"已有同内容文档"：返回库中存在的文档 + 重复错误
			return &model.Document{ID: "exist-" + filename, Filename: filename, Status: "ready", PageCount: 9, SizeBytes: size}, service.ErrDuplicateDocument
		}
	}
	if err, ok := f.errs[filename]; ok {
		return &model.Document{ID: "fail-" + filename, Filename: filename, Status: "failed", Error: err.Error(), SizeBytes: size}, err
	}
	return &model.Document{ID: "ok-" + filename, Filename: filename, Status: "ready", PageCount: 5, SizeBytes: size}, nil
}

func (f *fakeIngester) Get(context.Context, string) (*model.Document, error) { return nil, nil }
func (f *fakeIngester) List(context.Context) ([]model.Document, error)       { return nil, nil }
func (f *fakeIngester) Delete(context.Context, string) error                 { return nil }

func TestHandler_UploadMultipleFiles(t *testing.T) {
	ingester := &fakeIngester{errs: map[string]error{"bad.pdf": errors.New("PDF 解析失败: test")}}
	h := httpapi.NewHandler(ingester, nil, 20)
	r := httpapi.NewRouter(h)

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	for _, file := range []struct{ name, content string }{
		{"a.pdf", "%PDF-1.4 fake"},
		{"b.pdf", "%PDF-1.4 fake"},
		{"bad.pdf", "not a pdf"},
	} {
		fw, _ := w.CreateFormFile("file", file.name)
		_, _ = fw.Write([]byte(file.content))
	}
	_ = w.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/documents", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp struct {
		Total        int                   `json:"total"`
		SuccessCount int                   `json:"success_count"`
		FailedCount  int                   `json:"failed_count"`
		Results      []model.UploadResult  `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Total != 3 || resp.SuccessCount != 2 || resp.FailedCount != 1 {
		t.Fatalf("统计不对: total=%d success=%d failed=%d", resp.Total, resp.SuccessCount, resp.FailedCount)
	}

	byName := make(map[string]model.UploadResult, len(resp.Results))
	for _, r := range resp.Results {
		byName[r.Filename] = r
	}
	for _, name := range []string{"a.pdf", "b.pdf"} {
		if !byName[name].Success || byName[name].DocumentID == "" {
			t.Fatalf("%s 应成功: %+v", name, byName[name])
		}
	}
	if byName["bad.pdf"].Success || byName["bad.pdf"].Error == "" {
		t.Fatalf("bad.pdf 应失败且带原因: %+v", byName["bad.pdf"])
	}
	if byName["bad.pdf"].DurationMS < 0 {
		t.Fatalf("duration_ms 不应为负")
	}
}

func TestHandler_UploadDuplicate(t *testing.T) {
	ingester := &fakeIngester{dups: map[string]error{"dup.pdf": service.ErrDuplicateDocument}}
	h := httpapi.NewHandler(ingester, nil, 20)
	r := httpapi.NewRouter(h)

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	fw, _ := w.CreateFormFile("file", "dup.pdf")
	_, _ = fw.Write([]byte("%PDF-1.4 fake"))
	_ = w.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/documents", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp struct {
		Total          int                  `json:"total"`
		SuccessCount   int                  `json:"success_count"`
		FailedCount    int                  `json:"failed_count"`
		DuplicateCount int                  `json:"duplicate_count"`
		Results        []model.UploadResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Total != 1 || resp.SuccessCount != 0 || resp.FailedCount != 0 || resp.DuplicateCount != 1 {
		t.Fatalf("统计不对: %+v", resp)
	}
	item := resp.Results[0]
	if !item.Duplicate || item.Success || item.Error == "" {
		t.Fatalf("重复项标记不对: %+v", item)
	}
	if item.DocumentID != "exist-dup.pdf" {
		t.Fatalf("重复项应带已有文档 id: %+v", item)
	}
}

func TestHandler_UploadNoFile(t *testing.T) {
	h := httpapi.NewHandler(&fakeIngester{}, nil, 20)
	r := httpapi.NewRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/documents", bytes.NewBufferString("no file"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("无文件时 status = %d, want 400", rec.Code)
	}
}
