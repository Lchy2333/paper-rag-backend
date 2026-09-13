// Package render 提供 PDF 页面渲染与公式区域截图能力。
//
// 背景：公式 OCR 需要"公式区域的截图"，而 ledongthuc/pdf 是纯文本提取器，
// 没有渲染能力。这里用 PyMuPDF（MuPDF 的 Python 绑定，pip install pymupdf）
// 按公式检测得到的坐标边界（PDF 点，Y 向上）直接渲染出公式区域 PNG。
//
// 坐标换算：parser 的坐标原点在左下、Y 向上；PyMuPDF 的 clip 原点在左上、
// Y 向下，所以 clipY = 页高 - PDF_Y。
package render

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Region 是页面上的一个矩形区域（PDF 坐标，原点左下，Y 向上，单位 pt）。
type Region struct {
	Left  float64
	Right float64
	Top   float64
	Bot   float64
}

// Config 控制渲染行为。
type Config struct {
	// PythonCmd Python 可执行文件，空用 "python"。
	PythonCmd string
	// DPI 渲染分辨率。
	DPI int
	// WorkDir 渲染临时文件目录，空用系统临时目录。
	WorkDir string
}

// CropPage 用 PyMuPDF 把 PDF 第 page 页（1-based）中 region 指定的区域渲染成 PNG 字节。
func (c Config) CropPage(pdfPath string, page int, region Region) ([]byte, error) {
	if c.DPI <= 0 {
		c.DPI = 150
	}
	pythonCmd := c.PythonCmd
	if pythonCmd == "" {
		pythonCmd = "python"
	}

	workDir := c.WorkDir
	if workDir == "" {
		workDir = os.TempDir()
	}
	scriptPath := filepath.Join(workDir, fmt.Sprintf("formula_render_%d.py", os.Getpid()))
	outPath := filepath.Join(workDir, fmt.Sprintf("formula_crop_%d_%d.png", os.Getpid(), page))
	defer os.Remove(scriptPath)
	defer os.Remove(outPath)

	if err := os.WriteFile(scriptPath, []byte(renderScript), 0o600); err != nil {
		return nil, fmt.Errorf("写渲染脚本失败: %w", err)
	}

	args := []string{
		scriptPath,
		pdfPath,
		fmt.Sprint(page),
		fmt.Sprint(c.DPI),
		fmt.Sprint(region.Left),
		fmt.Sprint(region.Right),
		fmt.Sprint(region.Top),
		fmt.Sprint(region.Bot),
		outPath,
	}
	var stderr bytes.Buffer
	cmd := exec.Command(pythonCmd, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("PyMuPDF 渲染失败: %v: %s", err, stderr.String())
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("读取公式截图失败: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("公式截图为空: %+v", region)
	}
	return data, nil
}

// renderScript 是 PyMuPDF 渲染脚本：
// 参数顺序：pdf 页码 dpi left right top bot 输出路径（region 为 PDF 坐标，Y 向上）。
const renderScript = `import sys
import fitz

pdf_path, page_no, dpi = sys.argv[1], int(sys.argv[2]), float(sys.argv[3])
left, right, top, bot = (float(x) for x in sys.argv[4:8])
out_path = sys.argv[8]

doc = fitz.open(pdf_path)
page = doc[page_no - 1]
h = page.rect.height
scale = dpi / 72.0

# parser 坐标 Y 向上、原点左下 → PyMuPDF clip 坐标 Y 向下、原点左上
clip = fitz.Rect(left, h - top, right, h - bot)
mat = fitz.Matrix(scale, scale)
pix = page.get_pixmap(matrix=mat, clip=clip)
pix.save(out_path)
doc.close()
`
