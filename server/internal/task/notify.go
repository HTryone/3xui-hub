package task

// 单用户通知接缝：给「关闭流量」这类业务动作（BanService）与「解锁防重标记」
// （MailNotifyService::restoreReminders）复用同一套渲染 + 防重 + 发信口径。
//
// 为什么单独抽出来：渲染/防重/入队全在 commands.go 的 commandRunner 里（邮件扫描用），
// 而 collector（封禁/关闭流量）不能 import task（task 已经 import collector，会成环）。
// 所以 collector 只认一个两三个方法的小接口（collector.MailNotifier），
// 实现在这里，由 cmd/server 组装时注入。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// NotifyScenes 是自动通知的全部场景（与 PHP MailNotifyService::SCENES 同集合）。
//
// 顺序无关紧要，用于 restoreReminders 一次性清掉该用户的全部防重标记。
var NotifyScenes = []string{"traffic_exhausted", "traffic_almost", "expiring", "expired"}

// SceneNotifier 按场景给单个用户发通知（对齐 MailNotifyService::render +
// SendMailJob::dispatch）。
type SceneNotifier struct {
	r *commandRunner
}

// NewSceneNotifier 构造单用户通知器（deps 与 CommandRunner 同一份即可）。
func NewSceneNotifier(deps CommandDeps) *SceneNotifier {
	logf := deps.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &SceneNotifier{r: &commandRunner{deps: deps, logf: logf, now: now}}
}

// NotifyScene 发一条场景通知。
//
// 与扫描路径完全同口径：场景开关未开 / 渲染不出模板 / 没有收件人 / 本时代已发过
// → 静默跳过（返回 nil）。关流量是高频动作（超时用户每 recheck_after_hours 会被重关
// 一次），这里绝不能因为「已经发过」报错打断封禁流程。
func (n *SceneNotifier) NotifyScene(ctx context.Context, scene string, userID int64) error {
	if n == nil || n.r == nil || n.r.deps.DB == nil {
		return nil
	}
	// 开关必须先判：dispatchNotifyOnce 只做渲染 + 防重 + 入队，不认识「场景是否启用」
	// （扫描路径里 isEnabled 也是各自判的）。漏这一步，未开启的通道也会发信。
	if !n.r.notifyEnabled(ctx, scene) {
		return nil
	}
	u, err := n.r.loadNotifyUser(ctx, userID)
	if err != nil {
		return err
	}
	if u == nil {
		return nil
	}
	return n.r.dispatchNotifyOnce(ctx, scene, u)
}

// ForgetReminders 解锁该用户的全部自动通知防重标记（对齐
// MailNotifyService::restoreReminders）。
//
// 用在「购买 / 续费 / 重置流量 / 管理员改套餐」之后：不解除标记，该用户同一场景
// 一辈子只会收到第一封（标记 TTL 365 天）。
func (n *SceneNotifier) ForgetReminders(ctx context.Context, userID int64) error {
	if n == nil || n.r == nil || n.r.deps.DB == nil || userID <= 0 {
		return nil
	}
	var email string
	err := n.r.deps.DB.QueryRowContext(ctx, "SELECT email FROM users WHERE id = ?", userID).Scan(&email)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}

	var firstErr error
	for _, scene := range NotifyScenes {
		// 收件人是用户本人时按邮箱去重；管理员通知按触发用户记（admin:{id}）。
		// 两个维度都要清，否则只放开了「发给用户」的那一路。
		keys := []string{
			"mail_notify:" + scene + ":email:" + strings.ToLower(strings.TrimSpace(email)),
			fmt.Sprintf("mail_notify:%s:admin:%d", scene, userID),
		}
		for _, key := range keys {
			if err := n.r.cacheForget(ctx, key); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// loadNotifyUser 按 id 读一行通知用户（不存在返回 (nil, nil)）。
func (r *commandRunner) loadNotifyUser(ctx context.Context, userID int64) (*notifyUser, error) {
	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, email, token, traffic_limit, traffic_used, expired_at,
		        plan_id, monthly_traffic_used, monthly_traffic_limit
		 FROM users WHERE id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	return scanNotifyUser(rows)
}
