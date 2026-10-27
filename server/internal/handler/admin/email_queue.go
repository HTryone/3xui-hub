package admin

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// mailQueue 是 MailQueue 的 goroutine 实现：单 goroutine 串行消费 + DelaySeconds 延迟。
//
// 对齐 PHP 语义（SendMailJob::onQueue + queue:work 单线程）：
//   - 单 goroutine 一封一封发，避免 4 并发触发 SMTP 风控/限流；
//   - DelaySeconds = (i/perMinute)*60 秒在取到 job 后 sleep（对齐 ->delay()），
//     限速真正生效，不会一口气全发；
//   - 发信执行体由 MailDomain.deliver 提供（load 配置 + 发信 + 写 mail_logs + 失败语义）。
type mailQueue struct {
	jobs    chan MailJob
	deliver func(job MailJob)
	logger  loggerLike

	// interval 返回两封之间的间隔（读 site_config 的 mail_send_interval_ms）；
	// sleep 可注入（测试里替换成记账不真睡）。
	interval func() time.Duration
	sleep    func(time.Duration)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// newMailQueue 构造并启动单 worker 的邮件队列。
//
// interval 为空时按 0 处理（不发间隔），sleep 为空时用 time.Sleep。
func newMailQueue(deliver func(MailJob), logger loggerLike, interval func() time.Duration) *mailQueue {
	ctx, cancel := context.WithCancel(context.Background())
	q := &mailQueue{
		jobs:     make(chan MailJob, 256),
		deliver:  deliver,
		logger:   logger,
		interval: interval,
		sleep:    time.Sleep,
		ctx:      ctx,
		cancel:   cancel,
	}
	q.wg.Add(1)
	go q.worker()
	return q
}

// Enqueue 实现 MailQueue（PHP 的 SendMailJob::dispatch）。
func (q *mailQueue) Enqueue(job MailJob) error {
	select {
	case <-q.ctx.Done():
		return fmt.Errorf("邮件队列已停止")
	case q.jobs <- job:
		return nil
	}
}

// worker 单 goroutine 消费：DelaySeconds 延迟 + 串行发信 + 封间隔。
//
// 间隔在 deliver 之后、取下一封之前生效（mail_send_interval_ms，仅 >0 才 sleep）；
// 每封各自读配置，改配置后下一封立即生效。
func (q *mailQueue) worker() {
	defer q.wg.Done()
	for {
		select {
		case <-q.ctx.Done():
			return
		case job := <-q.jobs:
			if job.DelaySeconds > 0 {
				q.sleep(time.Duration(job.DelaySeconds) * time.Second)
			}
			q.deliver(job)
			if d := q.currentInterval(); d > 0 {
				q.sleep(d)
			}
		}
	}
}

// currentInterval 读一次 mail_send_interval_ms；空/坏值/负数按 0（不等）处理。
func (q *mailQueue) currentInterval() time.Duration {
	if q.interval == nil {
		return 0
	}
	return q.interval()
}

// Stop 停止接收并等待在发的一封结束（进程退出时由 main 调用）。
func (q *mailQueue) Stop() {
	q.cancel()
	q.wg.Wait()
}

// deliver 是单封信的发送执行体（对齐 SendMailJob::handle）：
// load 配置 → 挑号发信（多账号池，见 smtp_pool.go）→ 写 mail_logs（成功 sent / 失败 failed+error）。
//
// SMTP 4xx/5xx 与网络错误都归为 failed 并落日志（PHP 侧 UnexpectedResponseException
// 400~599 判永久失败不重试、网络错误 releaseClaim 下轮补发 —— 这里单队列一次送达，
// 失败即记 failed，不无限重试，避免死信反复占用队列）。
//
// 账号池只在「有可用号」时生效；池空时回落到站点配置的 smtp_*，行为与上线前一致。
func (d *MailDomain) deliver(job MailJob) {
	ctx := context.Background()
	cfg, err := d.store.MailConfigMany(ctx, emailConfigKeys)
	if err != nil {
		d.logger.Printf("admin mail deliver 读配置失败: %v", err)
		return
	}
	result := d.deliverWithPool(ctx, job, cfg)
	sendErr := result.sendErr

	now := d.now().UTC()
	var toUserID *int64
	if job.UserID != nil {
		v := int64(*job.UserID)
		toUserID = &v
	}
	row := MailLogRow{
		Type:      job.Type,
		ToEmail:   &job.ToEmail,
		ToUserID:  toUserID,
		Subject:   &job.Subject,
		Scene:     job.Scene,
		BatchID:   job.BatchID,
		CreatedAt: &now,
	}
	if sendErr != nil {
		row.Status = mailLogStatusFailed
		em := sendErr.Error()
		row.Error = &em
		d.logger.Printf("admin mail deliver 发信失败: %v", sendErr)
	} else {
		row.Status = mailLogStatusSent
	}
	if err := d.store.CreateMailLog(ctx, row); err != nil {
		d.logger.Printf("admin 写发信日志失败: %v", err)
	}
}
