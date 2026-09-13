// Command formula_ocr 是公式 OCR 的独立验证工具：
// 输入一张公式裁剪图，输出模型识别的 LaTeX。
//
// 用法：
//
//	go run ./cmd/formula_ocr -image 公式.png [-config config/config.yaml]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"paper-rag-backend/internal/ai"
	"paper-rag-backend/internal/config"
)

func main() {
	configPath := flag.String("config", "config/config.yaml", "配置文件路径")
	imagePath := flag.String("image", "", "公式图片路径")
	flag.Parse()

	if *imagePath == "" {
		log.Fatal("please specify an image path with -image")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config failed: %v", err)
	}

	img, err := os.ReadFile(*imagePath)
	if err != nil {
		log.Fatalf("read image failed: %v", err)
	}

	client := ai.NewClient(cfg.AI)
	latex, err := client.FormulaOCR(context.Background(), img)
	if err != nil {
		log.Fatalf("formula OCR failed: %v", err)
	}

	fmt.Println(latex)
}
