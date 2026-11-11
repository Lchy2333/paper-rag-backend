// Package span 是阶段耗时秒表（P95 分析的数据源）。
//
// 与 Agent 仓 app/span.py 同语义、同字段：
//
//	msg=span name=<阶段名> dur=<毫秒> ok=<0|1> rid=<链路id或-> run_id=<压测批次或-> [key=value...]
//
// 设计约定：
//   - 聚合脚本按 name 分组算阶段 P95，run_id 切片隔离压测流量；
//   - dur 一律 time.Since(start) 本地算（跨服务不相减时间戳）；
//   - rid/run_id 走标准 context 传播（Go 无 contextvar，必须显式注入）：
//     httpapi.RequestID 中间件调 WithRequest 注入 → service/store/ai 层从 ctx 读；
//   - worker（异步摄入）没有请求 ctx → rid 记 "-"，附加 doc_id 作关联键；
//   - 判定一个阶段要不要 span：这步慢了我会不会去优化它。
package span

import (
	"context"
	"time"

	"paper-rag-backend/internal/logger"
)

type ctxKey int

const (
	ridKey ctxKey = iota
	runIDKey
)

// WithRequest 把 rid/run_id 注入标准 context（httpapi.RequestID 中间件调用）。
// gin 的 c.Keys 只在 handler 层可达；service/store/ai 层签名全是 context.Context，
// 不塞进 ctx 的话 rid 就断在 handler 边界，业务层 span 行只能记 "-"。
func WithRequest(ctx context.Context, rid, runID string) context.Context {
	ctx = context.WithValue(ctx, ridKey, rid)
	return context.WithValue(ctx, runIDKey, runID)
}

// Log 打一行 span 日志。err 非 nil 时 ok=0；extra 为附加键值对（slog kv，如 "doc_id", id）。
//
// 调用惯例（替换原有的散装 duration_ms Debug 行）：
//
//	start := time.Now()
//	result, err := doSomething(ctx)
//	span.Log(ctx, "rag.embed_query", start, err, "hits", len(result))
//	if err != nil { ... }   // 错误处理仍在调用方
func Log(ctx context.Context, name string, start time.Time, err error, extra ...any) {
	ok := 1
	if err != nil {
		ok = 0
	}
	rid, _ := ctx.Value(ridKey).(string)
	runID, _ := ctx.Value(runIDKey).(string)
	if rid == "" {
		rid = "-"
	}
	if runID == "" {
		runID = "-"
	}
	args := make([]any, 0, 10+len(extra))
	args = append(args,
		"name", name,
		"dur", time.Since(start).Milliseconds(),
		"ok", ok,
		"rid", rid,
		"run_id", runID,
	)
	args = append(args, extra...)
	logger.Info("span", args...)
}
