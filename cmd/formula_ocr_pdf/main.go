// Command formula_ocr_pdf 是公式识别的整链路验证工具：
// 输入 PDF → 检测公式区域 → PyMuPDF 渲染整页 → 裁剪公式区域 → 视觉模型 OCR → 输出 LaTeX。
//
// 用法：
//
//	go run ./cmd/formula_ocr_pdf -pdf 公式.pdf [-config config/config.yaml] [-page 1]
//	go run ./cmd/formula_ocr_pdf -pdf 公式.pdf -detect   # 只打印检测结果，不调 OCR
//
// 需要先安装 PyMuPDF（pip install pymupdf）并配置 render.python_cmd。
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/ledongthuc/pdf"

	"paper-rag-backend/internal/ai"
	"paper-rag-backend/internal/config"
	"paper-rag-backend/internal/parser"
	"paper-rag-backend/internal/render"
)

// openPDF 复用 parser 的容错：截断 %%EOF 后的附加元数据再解析（同 parser.trimTrailingJunk）。
func openPDF(path string) (*os.File, *pdf.Reader, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	idx := bytes.LastIndex(raw, []byte("%%EOF"))
	if idx >= 0 {
		end := idx + len("%%EOF")
		if end < len(raw) {
			raw = raw[:end]
		}
	}
	size := int64(len(raw))
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	r, err := pdf.NewReader(io.NewSectionReader(f, 0, size), size)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, r, nil
}

func main() {
	configPath := flag.String("config", "config/config.yaml", "配置文件路径")
	pdfPath := flag.String("pdf", "", "PDF 路径")
	pageNum := flag.Int("page", 0, "只处理指定页（1-based），0 表示全部页")
	detectOnly := flag.Bool("detect", false, "只打印检测结果，不调 OCR")
	flag.Parse()

	if *pdfPath == "" {
		log.Fatal("please specify a PDF path with -pdf")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config failed: %v", err)
	}

	client := ai.NewClient(cfg.AI)
	renderCfg := render.Config{
		PythonCmd: cfg.Render.PythonCmd,
		DPI:       cfg.Render.DPI,
		WorkDir:   cfg.Render.WorkDir,
	}

	f, reader, err := openPDF(*pdfPath)
	if err != nil {
		log.Fatalf("open PDF failed: %v", err)
	}
	defer f.Close()

	pages := reader.NumPage()
	if *pageNum > 0 {
		pages = *pageNum
	}

	for p := 1; p <= pages; p++ {
		page := reader.Page(p)
		content := page.Content()
		formulas := parser.DetectFormulas(content)
		if len(formulas) == 0 {
			continue
		}
		for i, fr := range formulas {
			region := render.Region{
				Left:  fr.Left,
				Right: fr.Right,
				Top:   fr.Top,
				Bot:   fr.Bot,
			}
			fmt.Printf("page %d formula %d  flat_text=[%s]  bbox(%.0f,%.0f,%.0f,%.0f)\n",
				p, i+1, strings.ReplaceAll(fr.Text, "\n", " / "), fr.Left, fr.Right, fr.Top, fr.Bot)
			if *detectOnly {
				continue
			}
			img, err := renderCfg.CropPage(*pdfPath, p, region)
			if err != nil {
				log.Printf("page %d formula %d render failed: %v", p, i+1, err)
				continue
			}
			latex, err := client.FormulaOCR(context.Background(), img)
			if err != nil {
				log.Printf("page %d formula %d OCR failed: %v", p, i+1, err)
				continue
			}
			fmt.Printf("        →  LaTeX=[%s]\n", latex)
		}
	}
}
