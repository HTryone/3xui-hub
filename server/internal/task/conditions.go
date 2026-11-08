package task

// conditions.go 把「带 when 条件的任务」暴露成一个可查的函数，供系统状态页判断
// 「这条任务当前到底跑不跑」，从而把条件不满足的任务从列表里隐藏。
//
// 与调度器共用同一套条件实现（SQLLookups），保证「页面隐藏」与「实际不跑」永远一致：
// 条件为 false 的任务，调度器 tick 里也会被 When 拦掉、不执行。
//
// 目前只有 5 条任务带条件；其余任务没有条件（永远跑），这里返回 true。

import "context"

// ConditionChecker 返回一个「某任务当前条件是否成立」的判断函数。
//
// 说明：条件查询出错时返回 true（按「可能跑」处理），避免因一次 DB 抖动把任务从
// 页面误隐藏；调度器那边 tick 出错也会跳过执行并在日志里记，口径互不冲突。
func ConditionChecker(l ConditionLookups) func(ctx context.Context, name string) (bool, error) {
	return func(ctx context.Context, name string) (bool, error) {
		if l == nil {
			return true, nil
		}
		switch name {
		case NameMailNotifyScan:
			return l.MailNotifyAnyEnabled(ctx)
		case NameMailScheduleScan:
			return l.MailScheduleEnabled(ctx)
		case NameThirdPartyFetch:
			return l.ThirdPartyFetchEnabled(ctx)
		case NameXrayWarpRotate:
			return l.WarpRotateEnabled(ctx)
		case NameXrayRefreshOutbound:
			return l.OutboundSubsEnabled(ctx)
		default:
			return true, nil // 无条件任务：永远可能跑
		}
	}
}
