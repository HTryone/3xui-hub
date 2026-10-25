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

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// newMailQueue 构造并启动单 worker 的邮件队列。
func newMailQueue(deliver func(MailJob), logger loggerLike) *mailQueue {
	ctx, cancel := context.WithCancel(context.Background())
	q := &mailQueue{
		jobs:    make(chan MailJob, 256),
		deliver: deliver,
		logger:  logger,
		ctx:     ctx,
		cancel:  cancel,
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

// worker 单 goroutine 消费：DelaySeconds 延迟 + 串行发信。
func (q *mailQueue) worker() {
	defer q.wg.Done()
	for {
		select {
		case <-q.ctx.Done():
			return
		case job := <-q.jobs:
			if job.DelaySeconds > 0 {
				time.Sleep(time.Duration(job.DelaySeconds) * time.Second)
			}
			q.deliver(job)
		}
	}
}

// Stop 停止接收并等待在发的一封结束（进程退出时由 main 调用）。
func (q *mailQueue) Stop() {
	q.cancel()
	q.wg.Wait()
}

// deliver 是单封信的发送执行体（对齐 SendMailJob::handle）：
// load 配置 → 发信 → 写 mail_logs（成功 sent / 失败 failed+error）。
//
// SMTP 4xx/5xx 与网络错误都归为 failed 并落日志（PHP 侧 UnexpectedResponseException
// 400~599 判永久失败不重试、网络错误 releaseClaim 下轮补发 —— 这里单队列一次送达，
// 失败即记 failed，不无限重试，避免死信反复占用队列）。
func (d *MailDomain) deliver(job MailJob) {
	ctx := context.Background()
	cfg, err := d.store.MailConfigMany(ctx, emailConfigKeys)
	if err != nil {
		d.logger.Printf("admin mail deliver 读配置失败: %v", err)
		return
	}
	settings := d.buildSMTPSettings(cfg)

	sendErr := d.transport.Send(settings, job.ToEmail, job.Subject, job.HTMLBody)
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
