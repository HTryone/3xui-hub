package main

// 节点域异步任务的派发适配器（admin.NodeTaskDispatcher 的实现）。
//
// 背景：节点域（internal/handler/admin）不能 import internal/task，所以「把建好的
// async_tasks 派进队列」这一步由 cmd/server 注入。以前这一步整个缺失 —— 节点初始化、
// 入站同步、删除清理三类任务只落了库，永远停在 pending，面板上却提示「正在后台处理」。

import (
	"context"

	"controlhub-server/internal/handler/admin"
	"controlhub-server/internal/task"
)

// nodeTaskDispatcher 把节点任务派进队列。
type nodeTaskDispatcher struct {
	svc   *task.Service
	queue *task.Queue
}

// newNodeTaskDispatcher 构造派发器（svc 必须已 NewQueue 挂上队列）。
func newNodeTaskDispatcher(svc *task.Service, q *task.Queue) admin.NodeTaskDispatcher {
	return &nodeTaskDispatcher{svc: svc, queue: q}
}

// Dispatch 按类型把 pending 子项分批派出去（task.Service.Dispatch 自带
// node_init / sync_inbounds / cleanup_clients 的分支）。
func (d *nodeTaskDispatcher) Dispatch(ctx context.Context, taskID uint64) error {
	if d.svc == nil || taskID == 0 {
		return nil
	}
	t, err := d.svc.Get(ctx, int64(taskID))
	if err != nil {
		return err
	}
	if t == nil {
		return nil
	}
	return d.svc.Dispatch(ctx, t, nil)
}

// DispatchInboundScan 派发「入站同步扫描」（一台节点一个 Job，不占 item）。
//
// 对齐 PHP NodeInboundSyncService::submit：保存入站时只派这一个 Job，扫描时拉一次清单
// 做 diff —— 已经挂好的用户直接判完成，只有缺挂载的才逐个 attach。
func (d *nodeTaskDispatcher) DispatchInboundScan(_ context.Context, taskID, nodeID uint64) error {
	if d.queue == nil || taskID == 0 {
		return nil
	}
	d.queue.Enqueue(task.JobPayload{
		Kind:   task.JobNodeInboundScan,
		TaskID: int64(taskID),
		NodeID: int64(nodeID),
	})
	return nil
}

// compile-time 断言：节点域接口必须由本适配器满足。
var _ admin.NodeTaskDispatcher = (*nodeTaskDispatcher)(nil)
