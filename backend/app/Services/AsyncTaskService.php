<?php

namespace App\Services;

use App\Jobs\NodeClientsCleanupJob;
use App\Jobs\NodeInboundScanJob;
use App\Jobs\NodeInboundSyncJob;
use App\Jobs\SyncNodeTrafficJob;
use App\Jobs\NodeInitUserJob;
use App\Models\AsyncTask;
use App\Models\AsyncTaskItem;
use Illuminate\Support\Facades\Cache;
use Illuminate\Support\Facades\DB;
use InvalidArgumentException;

class AsyncTaskService
{
    /** @return array{task: AsyncTask, created: bool} */
    public function findOrCreateTrafficSync(
        array $nodeIds,
        ?string $subjectType = null,
        int|string|null $subjectId = null,
        ?int $userId = null,
    ): array {
        $scope = $subjectType === null ? 'all' : $subjectType.':'.$subjectId;

        return Cache::lock('traffic-sync-submit:'.$scope, 10)->block(3, function () use ($nodeIds, $subjectType, $subjectId, $userId) {
            $active = AsyncTask::query()
                ->where('type', 'traffic_sync')
                ->where('subject_type', $subjectType)
                ->where('subject_id', $subjectId)
                ->whereIn('status', [AsyncTask::STATUS_PENDING, AsyncTask::STATUS_RUNNING])
                ->latest('id')
                ->first();

            if ($active) {
                return ['task' => $active, 'created' => false];
            }

            $nodeIds = array_values(array_unique(array_map('intval', $nodeIds)));
            $meta = ['node_ids' => $nodeIds];
            if ($userId !== null) {
                $meta['user_id'] = $userId;
            }
            $task = $this->create(
                'traffic_sync',
                $subjectType,
                $subjectId,
                count($nodeIds),
                $meta,
                5,
                array_map(fn (int $nodeId) => 'node:'.$nodeId, $nodeIds),
            );

            if ($nodeIds === []) {
                $task = $this->completeEmpty($task);
            } else {
                $this->dispatchAfterCommit($task);
            }

            return ['task' => $task, 'created' => true];
        });
    }

    public function create(
        string $type,
        ?string $subjectType = null,
        int|string|null $subjectId = null,
        int $total = 0,
        ?array $meta = null,
        int $maxAttempts = 5,
        array $itemKeys = [],
    ): AsyncTask {
        if ($total < 0 || $maxAttempts < 1) {
            throw new InvalidArgumentException('任务参数无效');
        }

        if ($itemKeys === [] && $total > 0) {
            $itemKeys = array_map(fn (int $index) => 'item:'.$index, range(1, $total));
        }
        $itemKeys = array_values(array_unique($itemKeys));

        return DB::transaction(function () use ($type, $subjectType, $subjectId, $meta, $maxAttempts, $itemKeys) {
            $task = AsyncTask::create([
                'type' => $type,
                'status' => AsyncTask::STATUS_PENDING,
                'subject_type' => $subjectType,
                'subject_id' => $subjectId,
                'total' => count($itemKeys),
                'completed' => 0,
                'failed' => 0,
                'attempts' => 0,
                'max_attempts' => $maxAttempts,
                'meta' => $meta,
            ]);
            $this->addItems($task, $itemKeys);

            return $task;
        });
    }

    public function addItems(AsyncTask $task, array $itemKeys): void
    {
        $now = now();
        $rows = collect($itemKeys)->unique()->map(fn (string $itemKey) => [
            'task_id' => $task->id,
            'item_key' => $itemKey,
            'status' => AsyncTaskItem::STATUS_PENDING,
            'attempts' => 0,
            'created_at' => $now,
            'updated_at' => $now,
        ])->all();
        if ($rows !== []) {
            AsyncTaskItem::query()->insertOrIgnore($rows);
        }
        $task->update(['total' => $task->items()->count()]);
    }

    public function claimItem(int $taskId, string $itemKey): bool
    {
        return DB::transaction(function () use ($taskId, $itemKey) {
            $task = AsyncTask::query()->lockForUpdate()->findOrFail($taskId);
            $item = AsyncTaskItem::query()->where('task_id', $taskId)->where('item_key', $itemKey)->lockForUpdate()->firstOrFail();
            if ($item->status === AsyncTaskItem::STATUS_SUCCEEDED) {
                return false;
            }
            // P2-3：任务已被兜底/正常判终态（succeeded/failed）后，迟到的队列 Job 不再认领，
            // 避免复活并翻转 失败→成功。合法重试走 retry()（先把任务重置为 pending 再派发），不受影响。
            if (in_array($task->status, [AsyncTask::STATUS_SUCCEEDED, AsyncTask::STATUS_FAILED], true)) {
                return false;
            }
            if ($task->attempts >= $task->max_attempts && $task->status === AsyncTask::STATUS_PENDING) {
                throw new InvalidArgumentException('任务已达到最大尝试次数');
            }
            if ($task->status === AsyncTask::STATUS_PENDING) {
                $task->update([
                    'status' => AsyncTask::STATUS_RUNNING,
                    'attempts' => $task->attempts + 1,
                    'error' => null,
                    'started_at' => $task->started_at ?? now(),
                    'finished_at' => null,
                ]);
            }
            $item->update([
                'status' => AsyncTaskItem::STATUS_RUNNING,
                'attempts' => $item->attempts + 1,
                'error' => null,
                'started_at' => now(),
                'finished_at' => null,
            ]);

            return true;
        });
    }

    public function completeItem(int $taskId, string $itemKey): AsyncTask
    {
        return $this->finishItem($taskId, $itemKey, AsyncTaskItem::STATUS_SUCCEEDED);
    }

    public function failItem(int $taskId, string $itemKey, string $summary): AsyncTask
    {
        return $this->finishItem($taskId, $itemKey, AsyncTaskItem::STATUS_FAILED, $summary);
    }

    private function finishItem(int $taskId, string $itemKey, string $status, ?string $summary = null): AsyncTask
    {
        $task = DB::transaction(function () use ($taskId, $itemKey, $status, $summary) {
            $task = AsyncTask::query()->lockForUpdate()->findOrFail($taskId);
            $item = AsyncTaskItem::query()->where('task_id', $taskId)->where('item_key', $itemKey)->lockForUpdate()->firstOrFail();
            if (in_array($task->status, [AsyncTask::STATUS_SUCCEEDED, AsyncTask::STATUS_FAILED], true)) {
                return $task;
            }
            if ($item->status === AsyncTaskItem::STATUS_SUCCEEDED) {
                return $task;
            }
            $item->update([
                'status' => $status,
                'error' => $summary,
                'finished_at' => now(),
            ]);

            return $this->aggregateLocked($task);
        });

        return $task->refresh();
    }

    private function aggregateLocked(AsyncTask $task): AsyncTask
    {
        $counts = $task->items()->selectRaw('status, count(*) as aggregate')->groupBy('status')->pluck('aggregate', 'status');
        $completed = (int) ($counts[AsyncTaskItem::STATUS_SUCCEEDED] ?? 0);
        $failed = (int) ($counts[AsyncTaskItem::STATUS_FAILED] ?? 0);
        $active = (int) ($counts[AsyncTaskItem::STATUS_PENDING] ?? 0) + (int) ($counts[AsyncTaskItem::STATUS_RUNNING] ?? 0);
        $changes = ['total' => $task->items()->count(), 'completed' => $completed, 'failed' => $failed];
        if ($active === 0) {
            // P2-3 终态粘性：任务已被判终态后（超时兜底 failPendingItems / 上一次已完成），
            // 迟到的回执只刷新计数，不再翻转 status（避免 失败→成功 跳变）。
            $isTerminal = in_array($task->status, [AsyncTask::STATUS_SUCCEEDED, AsyncTask::STATUS_FAILED], true);
            if (!$isTerminal) {
                $changes['status'] = $failed > 0 ? AsyncTask::STATUS_FAILED : AsyncTask::STATUS_SUCCEEDED;
                $changes['error'] = $failed > 0 ? $this->summaryFor($task->type) : null;
                $changes['finished_at'] = now();
            }
        }
        $task->update($changes);
        return $task->refresh();
    }

    public function completeEmpty(AsyncTask $task): AsyncTask
    {
        $task->update(['status' => AsyncTask::STATUS_SUCCEEDED, 'total' => 0, 'completed' => 0, 'failed' => 0, 'error' => null, 'finished_at' => now()]);
        return $task->refresh();
    }

    /**
     * 超时兜底：把在阈值内未进入终态的任务标为失败，
     * 防止 Worker 被杀、Job 丢失、锁竞争等场景导致任务永久 running。
     * 阈值默认读 config('tasks.stale_after_minutes')。
     *
     * @param int|null $minutes 可选覆盖阈值（分钟），默认取配置
     * @return int 被清扫的任务数
     */
    public function timeoutStale(?int $minutes = null): int
    {
        $minutes = $minutes ?? (int) config('tasks.stale_after_minutes', 10);

        $staleTasks = AsyncTask::query()
            ->whereIn('status', [AsyncTask::STATUS_PENDING, AsyncTask::STATUS_RUNNING])
            ->where('updated_at', '<', now()->subMinutes($minutes))
            ->lockForUpdate()
            ->get();

        foreach ($staleTasks as $task) {
            $this->failPendingItems($task, '任务超时未完成');
        }

        return $staleTasks->count();
    }

    public function failPendingItems(AsyncTask $task, string $summary): AsyncTask
    {
        DB::transaction(function () use ($task, $summary) {
            $locked = AsyncTask::query()->lockForUpdate()->findOrFail($task->id);
            $locked->items()->whereIn('status', [AsyncTaskItem::STATUS_PENDING, AsyncTaskItem::STATUS_RUNNING])->update([
                'status' => AsyncTaskItem::STATUS_FAILED,
                'error' => $summary,
                'finished_at' => now(),
            ]);
            $this->aggregateLocked($locked);
        });
        return $task->refresh();
    }

    public function retry(AsyncTask $task): AsyncTask
    {
        [$retryTask, $failedKeys] = DB::transaction(function () use ($task) {
            $locked = AsyncTask::query()->lockForUpdate()->findOrFail($task->id);
            if ($locked->status !== AsyncTask::STATUS_FAILED) {
                throw new InvalidArgumentException('仅失败任务可以重试');
            }
            if ($locked->attempts >= $locked->max_attempts) {
                throw new InvalidArgumentException('任务已达到最大尝试次数');
            }
            $failedKeys = $locked->items()->where('status', AsyncTaskItem::STATUS_FAILED)->lockForUpdate()->pluck('item_key')->all();
            if ($failedKeys === []) {
                throw new InvalidArgumentException('没有可重试的失败项');
            }
            $locked->items()->whereIn('item_key', $failedKeys)->update([
                'status' => AsyncTaskItem::STATUS_PENDING,
                'error' => null,
                'started_at' => null,
                'finished_at' => null,
            ]);
            $locked->update([
                'status' => AsyncTask::STATUS_PENDING,
                'failed' => 0,
                'error' => null,
                'finished_at' => null,
                'updated_at' => now(),
            ]);

            return [$locked->refresh(), $failedKeys];
        });

        DB::afterCommit(function () use ($retryTask, $failedKeys) {
            // 入站同步的重试重新走一遍【扫描 + 差异】，而不是重放逐用户 Job。
            // 这个任务的定义就是「算差异」，逐用户 attach 只是执行手段：重放逐用户 Job 会对
            // 「面板上根本没有 client」的用户直接 attach 并报错（首次跑时这些项由扫描判为无需处理），
            // 重扫则以 1 次面板请求重新得出同样的结论，顺带把节点已经恢复的情况一次修好。
            if (self::isInboundSync($retryTask)) {
                NodeInboundScanJob::dispatch((int) $retryTask->subject_id, $retryTask->id);

                return;
            }
            $this->dispatch($retryTask, $failedKeys);
        });
        return $retryTask;
    }

    /** 判定任务是否为「节点入站同步」（type=node_action 且 meta.action=sync_inbounds）。 */
    private static function isInboundSync(AsyncTask $task): bool
    {
        return $task->type === 'node_action' && (string) ($task->meta['action'] ?? '') === 'sync_inbounds';
    }

    /**
     * 把「user:{id}」形状的 item 按批分组，产出每批的 [userIds, itemKeys]。
     *
     * 批大小统一读 config('tasks.node_sync_batch')：node_init 与 sync_inbounds 的 item 都是
     * 「1 个用户 × 1 台节点」，单个 Job 的成本结构完全一样（固定开销摊薄 + 面板往返不变），
     * 收益与失败半径的权衡因此一致，没有理由各配一套（多一个配置项就多一个会配歪的地方）。
     *
     * @param list<string> $itemKeys
     * @return list<array{userIds: list<int>, itemKeys: list<string>}>
     */
    private function userItemBatches(array $itemKeys): array
    {
        $batchSize = max(1, (int) config('tasks.node_sync_batch', 2));
        $batches = [];

        foreach (array_chunk(array_values($itemKeys), $batchSize) as $chunk) {
            $userIds = [];
            foreach ($chunk as $itemKey) {
                [, $id] = array_pad(explode(':', (string) $itemKey, 2), 2, null);
                $userIds[] = (int) $id;
            }

            $batches[] = ['userIds' => $userIds, 'itemKeys' => array_values($chunk)];
        }

        return $batches;
    }

    /**
     * 入站同步：把 item 按批分组，一个 Job 处理 1~2 个用户（批大小见 config/tasks.php）。
     *
     * 批处理只为摊薄「每个 Job 的固定开销」，每个用户该发的 attach 一次不少；
     * item 粒度不变，日志页仍然是逐用户一行、失败可单条重试。
     */
    private function dispatchSyncInboundBatches(AsyncTask $task, array $itemKeys): void
    {
        foreach ($this->userItemBatches($itemKeys) as $batch) {
            NodeInboundSyncJob::dispatch((int) $task->subject_id, $batch['userIds'], $task->id, $batch['itemKeys']);
        }
    }

    /**
     * 节点初始化：把 item 按批分组，一个 Job 初始化 1~2 个用户。
     *
     * 与入站同步同形：item 仍是 user:{id}，节点 id 在任务行 subject_id 上。
     * 102 个用户 = 51 个 Job（批大小 2），单 worker 下的串行等待减半；
     * 逐用户的可见性与重试粒度不变（每个 item 仍单独 claim/complete/fail）。
     */
    private function dispatchNodeInitBatches(AsyncTask $task, array $itemKeys): void
    {
        foreach ($this->userItemBatches($itemKeys) as $batch) {
            NodeInitUserJob::dispatch((int) $task->subject_id, $batch['userIds'], $task->id, $batch['itemKeys']);
        }
    }

    public function dispatch(AsyncTask $task, ?array $onlyKeys = null): void
    {
        $keys = $onlyKeys ?? $task->items()->where('status', AsyncTaskItem::STATUS_PENDING)->pluck('item_key')->all();
        $meta = $task->meta ?? [];

        // sync_inbounds 按批派发（见上），不走下面的逐 item 循环
        if (self::isInboundSync($task)) {
            if ($keys !== []) {
                $this->dispatchSyncInboundBatches($task, $keys);
            }

            return;
        }

        // node_init 同样按批派发（102 个用户 = 51 个 Job）；item_key=user:{userId}，
        // 节点 id 在任务行 subject_id 上。retry() 只重放失败项，走的也是这里 ——
        // 失败项按同样的批大小重新分组，重试路径与首跑同形，不额外分支。
        if ($task->type === 'node_init') {
            if ($keys !== []) {
                $this->dispatchNodeInitBatches($task, $keys);
            }

            return;
        }

        foreach ($keys as $itemKey) {
            [, $id] = array_pad(explode(':', $itemKey, 2), 2, null);
            match ($task->type) {
                'traffic_sync' => SyncNodeTrafficJob::dispatch((int) $id, $task->id, isset($meta['user_id']) ? (int) $meta['user_id'] : null, $itemKey),
                // domain_action：item_key=domain:{id}，动作在 meta.action；remove 时行已删，带 meta.domain 供助手用
                'domain_action' => \App\Jobs\ApplyDomainJob::dispatch(
                    (int) $task->subject_id,
                    (string) ($meta['action'] ?? 'apply'),
                    $task->id,
                    $itemKey,
                    isset($meta['domain']) ? (string) $meta['domain'] : null,
                ),
                // node_action：动作在 meta.action；节点 id 在任务行 subject_id 上
                // - sync_inbounds：item_key=user:{id}，已在方法开头按批派发（见 dispatchSyncInboundBatches）
                // - cleanup_clients：item_key=node:{id}，整台节点一个 item（行已删，靠 meta 快照）
                'node_action' => match ((string) ($meta['action'] ?? '')) {
                    'cleanup_clients' => NodeClientsCleanupJob::dispatch(
                        (int) $task->subject_id,
                        $task->id,
                        $itemKey,
                        is_array($meta['node_snapshot'] ?? null) ? $meta['node_snapshot'] : null,
                    ),
                    default => throw new InvalidArgumentException('不支持的节点操作：' . ($meta['action'] ?? 'unknown')),
                },
                default => throw new InvalidArgumentException("不支持任务类型：{$task->type}"),
            };
        }
    }

    public function dispatchAfterCommit(AsyncTask $task): void
    {
        DB::afterCommit(fn () => $this->dispatch($task));
    }

    public function summaryFor(string $type): string
    {
        return match ($type) {
            'traffic_sync' => '节点同步失败',
            'node_init' => '节点接入初始化失败',
            'node_action' => '节点操作失败',
            'domain_action' => '域名操作失败',
            default => '任务执行失败',
        };
    }

    public function publicError(AsyncTask $task): ?string
    {
        return $task->error === null ? null : $this->summaryFor($task->type);
    }

}
