package admin

// 发信挑号 + 失败分类（多 SMTP 账号池的执行体）。
//
// 行为契约（对应任务书需求 2/3/4）：
//   - 池里有可用号 → 按 last_used_at 最久未用优先挑；发失败按类别换号，单封最多 3 个号；
//   - 池里没有可用号（空表 / 全部非 active / 全部到日限额）→ 回落站点配置的 smtp_*，
//     失败语义与上线前完全一致（不换号、不重试）；
//   - 认证失败 → 该号 disabled；限流/临时 → 该号 cooling 10 分钟；收件人永久拒收 → 不换号。

import (
	"context"
	"time"
)

// selectSMTPAccount 从候选里挑下一个该用的号（already=本次已试过的 id）。
//
// 挑号口径（需求 2）：
//  1. 只考虑 usable 的（active / 冷却已到期的 cooling / 未到日限额）；
//  2. 按 last_used_at 最久未用优先，NULL（从未用过）排最前；
//  3. 同一时间并列时按 id 升序（候选本身已排好序，这里保持稳定）。
//
// 返回 nil 表示池里没有可用号 → 调用方回落站点配置。
func selectSMTPAccount(candidates []SMTPAccount, already map[uint64]struct{}, now time.Time) *SMTPAccount {
	var best *SMTPAccount
	for i := range candidates {
		acc := &candidates[i]
		if _, tried := already[acc.ID]; tried {
			continue
		}
		if !acc.usable(now) {
			continue
		}
		if best == nil || smtpLessRecentlyUsed(acc, best) {
			best = acc
		}
	}
	return best
}

// smtpLessRecentlyUsed 判断 a 是否比 b 更久没被用过（NULL 最久），并列时 id 小的优先。
func smtpLessRecentlyUsed(a, b *SMTPAccount) bool {
	switch {
	case a.LastUsedAt == nil && b.LastUsedAt == nil:
		return a.ID < b.ID
	case a.LastUsedAt == nil:
		return true
	case b.LastUsedAt == nil:
		return false
	}
	if !a.LastUsedAt.Equal(*b.LastUsedAt) {
		return a.LastUsedAt.Before(*b.LastUsedAt)
	}
	return a.ID < b.ID
}

// deliverAttempt 是一次发信的结果（供 deliver 写日志 + 打日志用）。
type deliverAttempt struct {
	sendErr  error
	account  *SMTPAccount // nil = 用的站点配置兜底
	attempts int          // 实际尝试次数（池号 + 兜底）
	failKind smtpFailure
}

// deliverWithPool 发一封信：先从账号池轮换挑号，池不可用则回落 smtp_* 配置。
func (d *MailDomain) deliverWithPool(ctx context.Context, job MailJob, cfg map[string]string) deliverAttempt {
	now := d.now().UTC()
	out := deliverAttempt{}
	tried := map[uint64]struct{}{}

	for out.attempts < smtpMaxRotations {
		candidates, err := d.store.SMTPAccountCandidates(ctx)
		if err != nil {
			// 挑号失败按「没有池」处理：回落兜底，别让一次 SQL 抖动把整封信丢掉。
			d.logger.Printf("admin mail 挑号失败，回落单账号配置: %v", err)
			break
		}
		acc := selectSMTPAccount(candidates, tried, now)
		if acc == nil {
			break
		}
		tried[acc.ID] = struct{}{}
		out.attempts++
		out.account = acc

		sendErr := d.transport.Send(acc.Settings(), job.ToEmail, job.Subject, job.HTMLBody)
		if sendErr == nil {
			out.sendErr = nil
			out.failKind = smtpFailureOther
			if err := d.store.SMTPAccountUsed(ctx, acc.ID, now); err != nil {
				d.logger.Printf("admin mail 记录发信成功失败 (id=%d): %v", acc.ID, err)
			}
			return out
		}

		out.sendErr = sendErr
		kind := classifySMTPFailure(sendErr.Error())
		out.failKind = kind
		d.logger.Printf("admin mail 账号 %d 发信失败（%s）: %v", acc.ID, kind, sendErr)

		if !kind.rotatable() {
			// 收件人永久拒收 / 未识别错误：换号也没用，直接记 failed。
			return out
		}
		if err := d.store.SMTPAccountFailed(ctx, acc.ID, kind, now); err != nil {
			d.logger.Printf("admin mail 记录账号失败态失败 (id=%d): %v", acc.ID, err)
		}
	}

	// 池里已经没有可用号（或本次已换满 3 个）：回落到站点配置的单套 smtp_*。
	// 兜底只发一次，失败即 failed —— 与多号池上线前的行为一致。
	settings := d.buildSMTPSettings(cfg)
	out.account = nil
	out.failKind = smtpFailureOther
	out.attempts++
	out.sendErr = d.transport.Send(settings, job.ToEmail, job.Subject, job.HTMLBody)
	return out
}
