package main

// 邮件提醒防重标记的解锁适配器（admin.AlertRestorer 的实现）。
//
// admin 包不 import internal/task，所以「购买 / 续费 / 重置流量 / 改套餐后解锁该用户
// 全部自动通知」这一步由 cmd/server 注入。以前这一步整个缺失：用户被管理员续费后，
// 同一场景（流量用尽 / 即将用尽 / 即将到期 / 已到期）的提醒再也不会发出
// （防重标记 TTL 365 天，不清就一直生效）。

import (
	"context"

	"controlhub-server/internal/handler/admin"
	"controlhub-server/internal/task"
)

// alertRestorer 把 task.SceneNotifier.ForgetReminders 适配成 admin.AlertRestorer。
type alertRestorer struct {
	notifier *task.SceneNotifier
}

// Restore 解锁该用户的邮件提醒防重标记（对齐 MailNotifyService::restoreReminders）。
func (a alertRestorer) Restore(ctx context.Context, userID uint64) error {
	if a.notifier == nil {
		return nil
	}
	return a.notifier.ForgetReminders(ctx, int64(userID))
}

// compile-time 断言：用户域的提醒解锁接口必须由本适配器满足。
var _ admin.AlertRestorer = alertRestorer{}
