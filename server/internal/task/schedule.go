// Package task 是常驻调度与异步任务体系：替换 Laravel 的 Schedule（routes/console.php）
// + queue + AsyncTaskService（async_tasks / async_task_items）。
//
// 两块职责：
//
//  1. 定时调度（robfig/cron）：console.php 的每一条 Schedule 都在这里登记
//     （名称、cron 表达式、withoutOverlapping、when 条件），见 defaults.go 的调度清单；
//  2. 异步任务体系（async.go + queue.go）：任务/子项状态机 + 可重试日志页 +
//     goroutine 队列（队列分离默认关闭 → 全部进 default 队列）。
//
// 时区：Laravel 的 config/app.php timezone=UTC，全项目调度同一口径。
// cron 固定 UTC（cron.WithLocation(time.UTC)），与 PHP 的 dailyAt('00:00') 对齐。
package task

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// Job 是一条定时任务的登记项（对齐 Schedule::command / Schedule::job 的一串链式调用）。
type Job struct {
	// Name 是调度名（PHP ->name(...)），也是 withoutOverlapping 的互斥键。
	Name string
	// Spec 是 5 字段 cron 表达式（分钟起，UTC）。
	Spec string
	// WithoutOverlap 对齐 ->withoutOverlapping()：上一轮没跑完时跳过本轮。
	WithoutOverlap bool
	// When 对齐 ->when(fn)：返回 false 则整条任务跳过（不空转）。
	// nil = 无条件执行。条件查询出错按「跳过」处理并记日志（与 PHP 条件抛错不跑同向）。
	When func(ctx context.Context) (bool, error)
	// Run 是任务体。返回错误只记日志，不影响后续调度。
	Run func(ctx context.Context) error
}

// Scheduler 是定时调度器（robfig/cron + withoutOverlapping 互斥）。
type Scheduler struct {
	cron *cron.Cron
	logf func(format string, args ...any)
	now  func() time.Time
	// db 用于写运行记录（scheduled_task_runs）与 worker 心跳（cache 表）；
	// nil（测试/无库场景）则只跑任务不落记录，不影响调度本身。
	db *sql.DB

	mu   sync.Mutex
	busy map[string]bool // name → 上一轮还在跑
}

// Options 是 Scheduler 的构造参数。
type Options struct {
	// Now 可注入（测试固定时间用）；nil → time.Now。
	Now func() time.Time
	// Logf 是日志出口（默认 log.Printf）。
	Logf func(format string, args ...any)
	// Location 是 cron 时区（nil → UTC，与 Laravel 同口径）。
	Location *time.Location
	// DB 用于写 scheduled_task_runs 运行记录与 worker 心跳（nil 则跳过落库）。
	DB *sql.DB
}

// NewScheduler 构造调度器。时区固定 UTC（PHP 全项目口径）。
func NewScheduler(opts Options) *Scheduler {
	loc := opts.Location
	if loc == nil {
		loc = time.UTC
	}
	logf := opts.Logf
	if logf == nil {
		logf = log.Printf
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Scheduler{
		// 秒字段关闭：PHP 的 everyFiveMinutes/dailyAt 都是分钟粒度。
		cron: cron.New(cron.WithLocation(loc), cron.WithParser(
			cron.NewParser(cron.Minute|cron.Hour|cron.Dom|cron.Month|cron.Dow|cron.Descriptor),
		)),
		logf: logf,
		now:  now,
		busy: map[string]bool{},
		db:   opts.DB,
	}
}

// Register 登记一条任务。名字重复时报错（console.php 的 name 是 withoutOverlapping 的键，必须唯一）。
func (s *Scheduler) Register(jobs ...Job) error {
	for _, j := range jobs {
		if j.Name == "" {
			return fmt.Errorf("task: 调度项缺 name")
		}
		if j.Run == nil {
			return fmt.Errorf("task: 调度项 %s 缺 run", j.Name)
		}
		if _, err := cron.ParseStandard(j.Spec); err != nil {
			return fmt.Errorf("task: 调度项 %s 的 cron 表达式非法 %q: %w", j.Name, j.Spec, err)
		}
		j := j
		if _, err := s.cron.AddFunc(j.Spec, func() { s.tick(context.Background(), j) }); err != nil {
			return fmt.Errorf("task: 登记调度项 %s 失败: %w", j.Name, err)
		}
	}
	return nil
}

// tick 是一次调度触发：when 条件 + withoutOverlapping + 执行。
func (s *Scheduler) tick(ctx context.Context, j Job) {
	// Laravel 的 ->when() 在触发时求值，条件为 false 整条跳过。
	if j.When != nil {
		ok, err := j.When(ctx)
		if err != nil {
			s.logf("[schedule] %s 条件查询失败，本轮跳过: %v", j.Name, err)
			return
		}
		if !ok {
			return
		}
	}

	// withoutOverlapping：上一轮还在跑就跳过（PHP 用 cache 锁跨进程互斥；
	// Go 是单二进制部署，进程内互斥即可覆盖）。
	if j.WithoutOverlap {
		s.mu.Lock()
		if s.busy[j.Name] {
			s.mu.Unlock()
			s.logf("[schedule] %s 上一轮未结束，跳过本轮", j.Name)
			return
		}
		s.busy[j.Name] = true
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.busy[j.Name] = false
			s.mu.Unlock()
		}()
	}

	start := s.now()
	err := j.Run(ctx)
	// 写运行记录（对齐 PHP RecordScheduledTaskRun listener）：
	// finished → error=null、duration_ms=runtime 毫秒；failed → duration_ms=0、error=异常消息。
	status := "finished"
	errText := ""
	durMS := int(s.now().Sub(start).Milliseconds())
	if err != nil {
		s.logf("[schedule] %s 执行失败: %v", j.Name, err)
		status = "failed"
		errText = truncateRunes(err.Error(), 1000)
		durMS = 0
	}
	s.recordRun(ctx, j.Name, status, errText, durMS)
	// ban-check 跑完写 worker 心跳（对齐 PHP BanCheckJob 的 Cache::put，20 分钟 TTL）。
	if j.Name == NameBanCheck {
		s.writeWorkerHeartbeat(ctx)
	}
	// 长任务留痕（备份/全量扫描是分钟级）。
	if d := s.now().Sub(start); d > 30*time.Second {
		s.logf("[schedule] %s 耗时 %s", j.Name, d)
	}
}

// recordRun 写一条 scheduled_task_runs 并保留每命令最近 5 条（对齐 RecordScheduledTaskRun::KEEP）。
func (s *Scheduler) recordRun(ctx context.Context, command, status, errText string, durationMS int) {
	if s.db == nil {
		return
	}
	var errVal any
	if errText != "" {
		errVal = errText
	}
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO scheduled_task_runs (command, status, error, duration_ms, ran_at) VALUES (?, ?, ?, ?, ?)",
		command, status, errVal, durationMS, s.now().UTC()); err != nil {
		s.logf("[schedule] 写运行记录失败（%s）: %v", command, err)
		return
	}
	// 保留最近 5 条（MySQL 不支持 DELETE ... LIMIT 的子查询直接用同表，需套一层）。
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM scheduled_task_runs
		WHERE command = ? AND id NOT IN (
			SELECT id FROM (
				SELECT id FROM scheduled_task_runs WHERE command = ? ORDER BY id DESC LIMIT 5
			) keep
		)`, command, command); err != nil {
		s.logf("[schedule] 清理运行记录失败（%s）: %v", command, err)
	}
}

// writeWorkerHeartbeat 写 cache 表的 controlhub:worker-heartbeat（对齐 PHP BanCheckJob）。
func (s *Scheduler) writeWorkerHeartbeat(ctx context.Context) {
	if s.db == nil {
		return
	}
	now := s.now().UTC()
	// 值用 Y-m-d H:i:s（PHP toDateTimeString 口径），WorkerHeartbeat 侧兼容 RFC3339 与它两种。
	val := now.Format("2006-01-02 15:04:05")
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO cache (`key`, `value`, `expiration`) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE `value` = VALUES(`value`), `expiration` = VALUES(`expiration`)",
		"controlhub:worker-heartbeat", val, now.Add(20*time.Minute).Unix()); err != nil {
		s.logf("[schedule] 写 worker 心跳失败: %v", err)
	}
}

// truncateRunes 按 rune 截断到 n 个字符（对齐 PHP mb_substr($error, 0, 1000)）。
func truncateRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}

// Start 启动调度（非阻塞）。ctx 取消后停止。
func (s *Scheduler) Start(ctx context.Context) {
	s.cron.Start()
	go func() {
		<-ctx.Done()
		s.logf("[schedule] 停止")
		s.Stop()
	}()
}

// Stop 停止调度（等待在跑的任务结束）。
func (s *Scheduler) Stop() {
	<-s.cron.Stop().Done()
}

// Entries 返回已登记的 (name, spec) 列表（诊断/测试用）。
func (s *Scheduler) Entries() []cron.Entry { return s.cron.Entries() }
