<?php

namespace App\Jobs;

use App\Services\AsyncTaskService;
use App\Services\NodeCleanupService;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;
use Illuminate\Support\Facades\Log;

/**
 * 删节点后的远端客户端清理（AsyncTask item，item_key=node:{id} —— 一台节点一个 item）。
 *
 * 节点行在派发后就被删了，所以连接信息随 meta 快照传进来（密文，见 NodeCleanupService）。
 * 清理结果一律落到 item：成功 → succeeded；不可达 / 列表失败 / 部分删除失败 → failed +
 * 具体文案（日志页可见、可重试）。
 *
 * 这里【不】靠队列自动重试来处理「节点不可达」：重试窗口（3 次 ≈ 7.5 分钟）大概率等不到
 * 节点恢复，却会把任务挂在 running，反而可能先被 async-task-timeout 判失败。
 * 让管理员看到明确失败原因、修好网络后手点重试，更可控。
 * $tries 只兜底 DB 抖动一类的意外异常。
 */
class NodeClientsCleanupJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public int $tries = 3;

    public array $backoff = [30, 120, 300];

    public function __construct(
        public int $nodeId,
        public ?int $taskId = null,
        public ?string $itemKey = null,
        public ?array $nodeSnapshot = null,
    ) {
        // 节点类 Job 走同一条可配置队列；默认 'default'，只有部署侧显式设了
        // PANEL_NODE_OPS_QUEUE 并起了对应 worker 才切走（理由见 config/panel.php）
        $this->onQueue(config('panel.node_ops_queue') ?: 'default');
    }

    public function handle(AsyncTaskService $tasks, NodeCleanupService $cleanup): void
    {
        if ($this->taskId !== null && $this->itemKey !== null) {
            if (!$tasks->claimItem($this->taskId, $this->itemKey)) {
                return; // 已被处理（幂等重放）
            }
        }

        $result = $cleanup->cleanupNode($this->nodeSnapshot ?? []);

        if ($this->taskId === null || $this->itemKey === null) {
            return;
        }

        if ($result['ok']) {
            Log::info('节点远端客户端清理完成', ['node_id' => $this->nodeId, 'result' => $result['summary']]);
            $tasks->completeItem($this->taskId, $this->itemKey);

            return;
        }

        $tasks->failItem($this->taskId, $this->itemKey, $result['summary']);
    }

    public function failed(\Throwable $exception): void
    {
        if ($this->taskId === null || $this->itemKey === null) {
            return;
        }

        app(AsyncTaskService::class)->failItem(
            $this->taskId,
            $this->itemKey,
            '节点远端客户端清理失败：' . $exception->getMessage()
        );
    }
}
