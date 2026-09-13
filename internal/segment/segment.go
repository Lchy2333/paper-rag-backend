// Package segment 提供中文分词能力，基于 gojieba（结巴分词 Go 版）。
//
// 用途：BM25 全文检索需要先分词。PostgreSQL 内置分词器不支持中文，
// 且 zhparser/SCWS 在 Windows 编译困难，因此用纯 Go 的 jieba 在应用层分词，
// 把分词结果以空格分隔存进 tsvector 列（或直接传给 ts_rank 检索）。
//
// jieba 词典在首次使用时加载（内存约几十 MB），用全局单例复用。
package segment

import (
	"strings"
	"sync"

	"github.com/yanyiwu/gojieba"
)

var (
	once sync.Once
	jieba *gojieba.Jieba
)

// getJieba 返回全局分词器单例（懒加载）。
func getJieba() *gojieba.Jieba {
	once.Do(func() {
		// 使用默认词典（embed 到二进制外的 dict 目录）；若相对路径找不到会报错。
		// gojieba 默认从 ./dict 加载，这里显式用其内部默认配置。
		jieba = gojieba.NewJieba()
	})
	return jieba
}

// Tokenize 将文本分词，过滤停用词与纯符号，返回有意义的词序列。
// 用 Search 模式（细粒度切分，召回更好，适合检索场景）。
func Tokenize(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	words := getJieba().CutForSearch(text, true) // true = 同时输出 HMM 新词
	var out []string
	for _, w := range words {
		w = strings.TrimSpace(w)
		if w == "" || len([]rune(w)) < 2 || isStopWord(w) {
			continue
		}
		out = append(out, w)
	}
	return out
}

// ToSearchString 分词后拼成空格分隔的字符串，供 tsvector/to_tsquery 使用。
func ToSearchString(text string) string {
	return strings.Join(Tokenize(text), " ")
}

// TokenizeEnglish 对纯英文/公式文本按空白和标点切分（不经 jieba，避免被误切）。
// 公式如 "V=0.5πtf'Dcotθ" 整串保留，利于字面匹配。
func TokenizeEnglish(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '，' || r == '。' ||
			r == '？' || r == '！' || r == '；' || r == '、'
	})
	var out []string
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if len([]rune(f)) >= 2 {
			out = append(out, f)
		}
	}
	return out
}

// isStopWord 判断是否为停用词（标点/语气词/单字符等无检索价值的词）。
func isStopWord(w string) bool {
	for _, r := range w {
		// 纯标点/符号词
		if (r < 0x4E00 || r > 0x9FFF) && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			// 非中文非字母非数字
			if r != '\'' && r != '·' && r != 'θ' && r != 'π' && r != 'α' && r != 'β' && r != 'γ' {
				return true
			}
		}
	}
	return false
}
