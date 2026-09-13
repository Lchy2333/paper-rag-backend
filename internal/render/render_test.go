package render

import (
	"os"
	"testing"
)

// TestCropPage 用黄金语料 PDF 渲染一个区域，验证 PyMuPDF 链路能产出 PNG。
// 需要机器已 pip install pymupdf；未装则跳过。
func TestCropPage(t *testing.T) {
	pdfPath := os.Getenv("GOLD_PDF")
	if pdfPath == "" {
		t.Skip("未设置 GOLD_PDF，跳过")
	}
	cfg := Config{PythonCmd: "python", DPI: 150}
	data, err := cfg.CropPage(pdfPath, 1, Region{Left: 50, Right: 300, Top: 700, Bot: 600})
	if err != nil {
		t.Fatalf("CropPage 失败: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("截图为空")
	}
	// PNG 魔数校验
	if len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("输出不是 PNG: %d bytes, 前8字节 %x", len(data), data[:8])
	}
}
