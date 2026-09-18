<?php

namespace App\Services;

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

        DB::afterCommit(fn () => $this->dispatch($retryTask, $failedKeys));
        return $retryTask;
    }

    public function dispatch(AsyncTask $task, ?array $onlyKeys = null): void
    {
        $keys = $onlyKeys ?? $task->items()->where('status', AsyncTaskItem::STATUS_PENDING)->pluck('item_key')->all();
        $meta = $task->meta ?? [];
        foreach ($keys as $itemKey) {
            [, $id] = array_pad(explode(':', $itemKey, 2), 2, null);
            match ($task->type) {
                'traffic_sync' => SyncNodeTrafficJob::dispatch((int) $id, $task->id, isset($meta['user_id']) ? (int) $meta['user_id'] : null, $itemKey),
                // node_init 的 item_key 是 user:{userId}，节点 id 在任务行 subject_id 上
                'node_init' => NodeInitUserJob::dispatch((int) $task->subject_id, (int) $id, $task->id, $itemKey),
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
            default => '任务执行失败',
        };
    }

    public function publicError(AsyncTask $task): ?string
    {
        return $task->error === null ? null : $this->summaryFor($task->type);
    }

}
