// Package worker 实现异步摄入的工作池。
//
// 设计要点（详见 AGENTS.md「异步摄入实现备忘」，方案 B 混合）：
//   - DB 是唯一事实源：documents.status 就是队列（pending=待处理，processing=处理中），
//     不另建 jobs 表；channel 门铃只负责唤醒，不是队列
//   - 门铃：容量 1 的 chan struct{} + 非阻塞发送，重复信号自动合并；丢铃最多晚一个
//     轮询周期（兜底轮询补上），永不丢任务
//   - 抢单：store.ClaimPendingTask（FOR UPDATE SKIP LOCKED），多 worker/多实例天然分工
//   - 崩溃恢复：启动时 Recover() 把 processing 重置回 pending；正常停机（Drain 成功）
//     不会留下 processing，Recover 是幂等 no-op
//   - 停机：取消 Start 的 ctx 停止抢单 → Drain 宽限等在途 → 超时重置 processing 由下次启动续跑
//
// 任务级 context 不随停机取消（宽限期内允许在途任务跑完），带 taskTimeout 防单任务卡死。
package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"paper-rag-backend/internal/logger"
	"paper-rag-backend/internal/model"
	"paper-rag-backend/internal/store"
)

// IngestRunner 是 worker 对摄入异步段的依赖抽象（由 *service.IngestService 实现，
// Run(ctx, docID)：解析 → 分块 → 公式OCR → 向量化 → 入库 → status=ready/failed）。
type IngestRunner interface {
	Run(ctx context.Context, docID string) error
}

// Pool 是异步摄入工作池。
type Pool struct {
	runner       IngestRunner
	store        store.Store
	kick         chan struct{}
	workers      int
	pollInterval time.Duration
	taskTimeout  time.Duration
	wg           sync.WaitGroup
}

// NewPool 创建工作池。workers ≤ 0 时默认 2（= 流水线并发上限，天然限流）。
func NewPool(runner IngestRunner, s store.Store, workers int) *Pool {
	if workers <= 0 {
		workers = 2
	}
	return &Pool{
		runner:       runner,
		store:        s,
		kick:         make(chan struct{}, 1),
		workers:      workers,
		pollInterval: 5 * time.Second,
		taskTimeout:  30 * time.Minute,
	}
}

// SetPollInterval 覆盖兜底轮询间隔（主要供测试缩短等待）。
func (p *Pool) SetPollInterval(d time.Duration) *Pool {
	if d > 0 {
		p.pollInterval = d
	}
	return p
}

// Kick 按门铃（非阻塞）。上传接口写完 pending 记录后调用，让 worker 立刻醒来抢单。
// 容量 1 + 非阻塞发送：重复铃自动合并，铃满了丢弃也不怕（兜底轮询会补）。
func (p *Pool) Kick() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// Start 启动 workers 个后台 goroutine。ctx 取消 = 停止抢单（在途任务允许跑完，见 Drain）。
func (p *Pool) Start(ctx context.Context) {
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go p.loop(ctx, i)
	}
	logger.Info("worker.started", "workers", p.workers, "poll_interval_ms", p.pollInterval.Milliseconds())
}

// Recover 启动恢复：把上次进程硬崩留下的 processing 悬案重置回 pending，交给本次的 worker 续跑。
// 正常停机（Drain）不会留下 processing，因此正常重启时这是幂等 no-op。应在 Start 前调用。
func (p *Pool) Recover() (int, error) {
	n, err := p.store.ResetProcessingTasks(context.Background())
	if err != nil {
		return 0, err
	}
	if n > 0 {
		logger.Warn("worker.recovered", "reset_to_pending", n)
	}
	return n, nil
}

// Drain 停机善后：宽限 timeout 等在途任务完成；超时则把 processing 重置回 pending，
// 由下次启动的 Recover + worker 续跑（与硬崩殊途同归：不管怎么停，重启后的世界一样）。
// 调用前应已取消 Start 传入的 ctx（停止抢单）。
func (p *Pool) Drain(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		logger.Info("worker.drained", "grace", timeout.String())
	case <-time.After(timeout):
		n, err := p.store.ResetProcessingTasks(context.Background())
		if err != nil {
			logger.Error("worker.drain_reset_failed", "err", err)
		} else {
			logger.Warn("worker.drain_timeout", "reset_to_pending", n, "grace", timeout.String())
		}
		// 不再等 wg：进程即将退出，仍在跑的任务由下次启动恢复兜底
	}
}

// loop 是单个 worker 的主循环：等唤醒（门铃/兜底超时/停机）→ 循环抢单直到队列空。
func (p *Pool) loop(ctx context.Context, id int) {
	defer p.wg.Done()
	l := logger.With("worker", id)
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.kick:
		case <-time.After(p.pollInterval):
		}
		if ctx.Err() != nil {
			return
		}
		p.claimAndProcess(ctx, l)
	}
}

// claimAndProcess 循环抢单处理，直到队列空（或 ctx 取消）。
// 一轮唤醒处理完队列里的所有任务（趁热打劫），空了才回去睡觉等下一次门铃/超时。
func (p *Pool) claimAndProcess(ctx context.Context, l *slog.Logger) {
	for {
		if ctx.Err() != nil {
			return
		}
		doc, err := p.store.ClaimPendingTask(ctx)
		if err != nil {
			l.Warn("worker.claim_failed", "err", err)
			return
		}
		if doc == nil {
			return // 队列空：回去睡觉（等门铃/兜底轮询）
		}
		p.processOne(doc, l)
	}
}

// processOne 执行单个任务。任务级 ctx 独立于停机 ctx（宽限期内允许跑完），
// taskTimeout 防单任务卡死流水线（超时 → 内部各阶段报错 → fail() 标记 failed）。
func (p *Pool) processOne(doc *model.Document, l *slog.Logger) {
	taskCtx, cancel := context.WithTimeout(context.Background(), p.taskTimeout)
	defer cancel()

	start := time.Now()
	l.Info("worker.task_start", "doc_id", doc.ID, "file", doc.Filename)
	if err := p.runner.Run(taskCtx, doc.ID); err != nil {
		l.Warn("worker.task_failed", "doc_id", doc.ID,
			"duration_ms", time.Since(start).Milliseconds(), "err", err)
		return
	}
	l.Info("worker.task_done", "doc_id", doc.ID,
		"duration_ms", time.Since(start).Milliseconds())
}
