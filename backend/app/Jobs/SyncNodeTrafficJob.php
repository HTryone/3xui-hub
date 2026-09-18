<?php

namespace App\Jobs;

use App\Drivers\NodeDriverFactory;

use App\Models\Node;
use App\Models\User;
use App\Services\AsyncTaskService;
use App\Services\BanService;
use App\Services\TrafficSyncService;
use Illuminate\Support\Facades\Log;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;

/**
 * 单节点流量同步（批量优化版）。
 * 一次 listInbounds() 拉取全节点 client 流量，内存匹配后批量写入。
 */
class SyncNodeTrafficJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public int $tries = 5;
    public array $backoff = [60, 300, 900, 3600];
    /** 锁竞争时已重新入队的次数（随 Job 序列化携带，上限由 config('tasks.max_lock_releases') 控制） */
    public int $releaseCount = 0;

    public function __construct(
        public int $nodeId,
        public ?int $taskId = null,
        public ?int $userId = null,
        public ?string $itemKey = null,
    ) {}

    public function handle(
        NodeDriverFactory $driverFactory,
        TrafficSyncService $sync,
        BanService $banService,
        AsyncTaskService $tasks,
    ): void {
        if ($this->taskId !== null && $this->itemKey !== null) {
            if (!$tasks->claimItem($this->taskId, $this->itemKey)) {
                return;
            }
        }

        /** @var Node|null $node */
        $node = Node::find($this->nodeId);
        if (!$node || !$node->enabled) {
            // P2-2：节点被删除/禁用时本次同步什么都没做，标"失败+原因"而非假绿"成功"。
            if ($this->taskId !== null && $this->itemKey !== null) {
                $reason = $node === null
                    ? '节点已删除，跳过同步'
                    : '节点已禁用，跳过同步';
                $tasks->failItem($this->taskId, $this->itemKey, $reason);
            }
            return;
        }

        try {
            if ($this->userId !== null) {
                $user = User::find($this->userId);
                if (!$user) {
                    if ($this->taskId !== null && $this->itemKey !== null) $tasks->completeItem($this->taskId, $this->itemKey);
                    return;
                }
                $email = $user->clientEmail();
                $result = $sync->syncUserNodeFromSource($user, $node, function () use ($driverFactory, $node, $email) {
                    $driver = $driverFactory->make($node);
                    // 数据层已按 email 去重（同 email 跨入站只取首值，避免 N 倍计费）
                    $stats = $driver->getClientStatsByEmail();
                    return $stats[$email] ?? null;
                });
                $deltaMap = [];
            } else {
                $result = $sync->syncNodeFromSource($node, function () use ($driverFactory, $node) {
                    $driver = $driverFactory->make($node);
                    // 数据层已按 email 去重（同 email 跨入站只取首值，避免 N 倍计费）
                    return $driver->getClientStatsByEmail();
                });
                $deltaMap = $result['deltaMap'];
            }
        } catch (\Throwable $e) {
            Log::error('SyncNodeTrafficJob failed', [
                'task_id' => $this->taskId,
                'item_key' => $this->itemKey,
                'node_id' => $this->nodeId,
                'user_id' => $this->userId,
                'attempts' => $this->attempts(),
                'error' => $e->getMessage(),
            ]);
            throw $e;
        }

        if (!$result['acquired']) {
            $maxReleases = (int) config('tasks.max_lock_releases', 5);
            if ($this->releaseCount < $maxReleases) {
                $this->releaseCount++;
                Log::info('SyncNodeTrafficJob lock contention, re-queueing', [
                    'task_id' => $this->taskId,
                    'item_key' => $this->itemKey,
                    'node_id' => $this->nodeId,
                    'release_count' => $this->releaseCount,
                ]);
                $this->release(30);
                return;
            }
            Log::warning('SyncNodeTrafficJob lock contention exceeded limit, failing item', [
                'task_id' => $this->taskId,
                'item_key' => $this->itemKey,
                'node_id' => $this->nodeId,
                'release_count' => $this->releaseCount,
            ]);
            if ($this->taskId !== null && $this->itemKey !== null) {
                $tasks->failItem($this->taskId, $this->itemKey, '节点锁长时间竞争，同步终止');
            }
            return;
        }

        // Ban检查（仅检查有流量变化的用户）
        if (!empty($deltaMap)) {
            $users = User::whereIn('id', array_keys($deltaMap))->with('plan')->get();
            foreach ($users as $user) {
                $fresh = $user->fresh();
                $fresh->load('plan');
                $banService->checkAfterSync($fresh);
            }
        }

        if ($this->taskId !== null && $this->itemKey !== null) {
            $tasks->completeItem($this->taskId, $this->itemKey);
        }
    }

    public function failed(\Throwable $exception): void
    {
        if ($this->taskId !== null && $this->itemKey !== null) {
            report($exception);
            $tasks = app(AsyncTaskService::class);
            $tasks->failItem($this->taskId, $this->itemKey, $tasks->summaryFor('traffic_sync'));
        }
    }
}
