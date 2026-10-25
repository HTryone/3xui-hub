package main

import (
	"controlhub-server/internal/handler/admin"
	"controlhub-server/internal/task"
)

// taskMailSender 把 admin.MailQueue（单 goroutine 串行队列）适配成 task.MailSender：
// mail:notify-scan / mail:schedule-scan 的定时通知邮件复用同一个串行队列，与
// 管理端群发/测试邮件挤同一条通道，保证「单并发发信 + DelaySeconds 限速」，
// 不会各自起 worker 并发触发 SMTP 风控。
type taskMailSender struct {
	q admin.MailQueue
}

// EnqueueMail 实现 task.MailSender（字段与 admin.MailJob 1:1 拷贝）。
func (t *taskMailSender) EnqueueMail(job task.MailJob) error {
	return t.q.Enqueue(admin.MailJob{
		ToEmail:  job.ToEmail,
		Subject:  job.Subject,
		HTMLBody: job.HTMLBody,
		Type:     job.Type,
		UserID:   job.UserID,
		BatchID:  job.BatchID,
		Scene:    job.Scene,
	})
}
