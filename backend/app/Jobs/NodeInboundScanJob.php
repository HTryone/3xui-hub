<?php

namespace App\Jobs;

use App\Models\AsyncTask;
use App\Models\Node;
use App\Services\AsyncTaskService;
use App\Services\NodeInboundSyncService;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;

/**
 * 节点入站变更 · 清单扫描（一台节点一个 Job，不占 AsyncTask 的 item）。
 *
 * 保存节点入站时只派这一个 Job：它先拉一次 clients/list，把「已经挂好的用户」就地判完成，
 * 只把「缺挂载的用户」交给 NodeInboundSyncJob 去 attach。
 * 逐用户 getClient 问「你在不在」的那 100 次往返，被这一次列表请求整体替代。
 *
 * 不占 item 的原因：item 是「看得见的进度单位」，而扫描本身的成败不是某个用户的成败；
 * 扫描失败时会把任务里所有待处理 item 一并标失败并写明原因（见 scanNode），
 * 任务结果照样可见、可重试。
 *
 * 队列：读 config('panel.node_ops_queue')，默认 'default'（与引入开关前一致）。
 */
class NodeInboundScanJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public int $tries = 3;

    public array $backoff = [30, 120, 300];

    public function __construct(
        public int $nodeId,
        public ?int $taskId = null,
    ) {
        $this->onQueue(config('panel.node_ops_queue') ?: 'default');
    }

    public function handle(AsyncTaskService $tasks, NodeInboundSyncService $sync): void
    {
        if ($this->taskId === null) {
            return;
        }

        /** @var AsyncTask|null $task */
        $task = AsyncTask::find($this->taskId);
        if ($task === null || in_array($task->status, [AsyncTask::STATUS_SUCCEEDED, AsyncTask::STATUS_FAILED], true)) {
            return; // 任务已判终态（超时兜底 / 上一次已完成），不再扫描
        }

        /** @var Node|null $node */
        $node = Node::find($this->nodeId);
        if ($node === null) {
            $tasks->failPendingItems($task, '节点已删除，入站同步中止');

            return;
        }

        $sync->scanNode($node, $task);
    }

    public function failed(\Throwable $exception): void
    {
        if ($this->taskId === null) {
            return;
        }

        $tasks = app(AsyncTaskService::class);
        $task = AsyncTask::find($this->taskId);
        if ($task === null) {
            return;
        }

        // 兜底：scanNode 自己已把「清单拉不到」写成任务失败，能走到这里的是 DB 抖动一类的意外异常
        $tasks->failPendingItems($task, '节点入站扫描失败：' . $exception->getMessage());
    }
}
