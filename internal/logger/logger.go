// Package logger 是服务统一的日志入口，基于标准库 log/slog 的结构化日志。
//
// 职责：
//   - 包级默认 logger：各层直接调用 logger.Info/Debug/Warn/Error，无需注入。
//   - 级别过滤：Init 时按配置设置全局级别（debug 打开详细日志，info 为生产默认）。
//   - 字段串联：With 返回带固定字段的子 logger（如 doc_id），为异步任务对账铺路。
package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Level 日志级别，对应配置 log.level 的取值。
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// levelVar 记录当前生效级别，Init / SetOutput 都基于它重建 logger。
var levelVar = &slog.LevelVar{}

// defaultLogger 是包级默认 logger，Init 前以 info 级别输出到 stdout。
var defaultLogger = newLogger(os.Stdout)

// Init 初始化全局日志级别。level 不合法时回退到 info。
// 建议在 main 里配置加载完成后立即调用（配置加载阶段的错误仍走标准库 log）。
func Init(level string) {
	lv := slog.LevelInfo
	switch Level(strings.ToLower(strings.TrimSpace(level))) {
	case LevelDebug:
		lv = slog.LevelDebug
	case LevelInfo:
		lv = slog.LevelInfo
	case LevelWarn:
		lv = slog.LevelWarn
	case LevelError:
		lv = slog.LevelError
	}
	levelVar.Set(lv)
}

// SetOutput 替换日志输出目标（主要用于测试捕获）。
func SetOutput(w io.Writer) {
	defaultLogger = newLogger(w)
}

// newLogger 创建带时间戳/级别/字段的 Text 格式 logger。
// 用 TextHandler（非 JSON），键值对可读性好，便于控制台直接查看。
func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: levelVar}))
}

// Info 记录一条 info 级日志。
func Info(msg string, args ...any) { defaultLogger.Info(msg, args...) }

// Debug 记录一条 debug 级日志（仅 log.level=debug 时输出）。
func Debug(msg string, args ...any) { defaultLogger.Debug(msg, args...) }

// Warn 记录一条 warn 级日志。
func Warn(msg string, args ...any) { defaultLogger.Warn(msg, args...) }

// Error 记录一条 error 级日志。
func Error(msg string, args ...any) { defaultLogger.Error(msg, args...) }

// With 返回带固定字段的子 logger，如 logger.With("doc_id", id).Info(...)。
// 适合把一个任务贯穿的上下文字段（doc_id、task_id 等）绑定到所有相关日志。
func With(args ...any) *slog.Logger { return defaultLogger.With(args...) }

// Infof 以 Printf 风格记录 info 级日志（仅用于不便用键值对的场景）。
func Infof(format string, args ...any) {
	defaultLogger.Info(fmt.Sprintf(format, args...))
}

// Debugf 以 Printf 风格记录 debug 级日志。
func Debugf(format string, args ...any) {
	defaultLogger.Debug(fmt.Sprintf(format, args...))
}
