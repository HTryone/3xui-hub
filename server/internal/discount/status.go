// Package discount 提供优惠码「有效状态」的唯一判定口径。
//
// 背景：discount_codes.status 这一列只应存**终态**（used_up 用满 / refreshed 邀请码已刷新 /
// expired 人工作废）。而「到没到截止时间」是随时间连续变化的量，绝不能靠库里一个固定值
// 表达 —— 一旦存进去就必然与现实漂移（过了期没人下单，就永远停在 active，列表一直显示「生效中」）。
//
// 因此凡是要展示、筛选、判断「这个码现在到底什么状态」的地方，一律调 EffectiveStatus() 现算，
// 不读库里那个可能过时的 status。这与「订单过期」那套口径一致（读时现算，不依赖定时任务）。
package discount

import "time"

// 与 schema 里 discount_codes.status 的取值一一对应。
const (
	StatusActive    = "active"    // 生效中
	StatusUsedUp    = "used_up"   // 已用完
	StatusExpired   = "expired"   // 已过期
	StatusRefreshed = "refreshed" // 已刷新（邀请码被新码取代）
)

// EffectiveStatus 现算一个优惠码此刻的有效状态。
//
//   - status 不是 active（used_up / refreshed / expired）→ 原样返回：这些是终态，
//     人工或系统定下的，不因时间推移「复活」。
//   - status == active 且 expires_at 非空、已到/已过截止时间 → expired。
//   - status == active 且已用次数达到上限、max_uses > 0 → used_up。
//   - 其余 → active。
//
// 边界口径：expires_at == now 按**已过期**处理（到点即失效）。两边都先 Truncate 到秒，
// 与库 DATETIME 的秒级精度对齐，避免亚秒差异造成「列表说过期、下单却能过」的裂缝。
// 方向上也只往「过期」偏：宁可提前一秒标过期，也不能显示生效中却被下单接口拒掉。
func EffectiveStatus(status string, expiresAt *time.Time, usedCount, maxUses int64, now time.Time) string {
	if status != StatusActive {
		return status
	}
	if expiresAt != nil && !now.Truncate(time.Second).Before(expiresAt.UTC().Truncate(time.Second)) {
		return StatusExpired
	}
	if maxUses > 0 && usedCount >= maxUses {
		return StatusUsedUp
	}
	return StatusActive
}
