<?php

namespace App\Services;

use App\Models\AsyncTask;
use App\Models\Node;
use App\Models\User;
use Illuminate\Support\Facades\DB;

/**
 * 节点初始化（P1-1/P1-2 修复：异步、按用户粒度）。
 *
 * 节点接入（NodeController::store）时同步完成节点+入站落库，
 * 再按用户逐条派发初始化 Job（AsyncTask，item_key=user:{id}）：
 * - 单用户失败：队列重试 3 次后仅该项标失败，不影响其余用户
 * - 全部成功 → 按 should_enable 启用节点；有失败 → 仅把节点标为 offline（不碰 enabled），
 *   管理端日志页可整任务重试
 *
 * enabled 是管理员的开关，初始化流程无权改写：否则 1 个用户建号失败会把整个节点
 * （连同其余已建号成功的用户）一起踢出订阅，且因 HealthCheckJob 只扫 enabled 节点而无法自愈。
 */
class NodeInitService
{
    public function __construct(private AsyncTaskService $tasks) {}

    /** 建初始化任务并按用户逐条派发（afterCommit 派发，事务内调用安全）。 */
    public function submit(Node $node, bool $shouldEnable): AsyncTask
    {
        $userIds = User::query()->pluck('id')->all();

        $task = $this->tasks->create(
            'node_init',
            'node',
            $node->id,
            count($userIds),
            ['should_enable' => $shouldEnable, 'node_name' => $node->name],
            3,
            array_map(fn (int $id) => "user:{$id}", $userIds),
        );

        if ($userIds === []) {
            // 无用户：直接判终态（succeeded），enable 交给 maybeFinalize 定
            $task = $this->tasks->completeEmpty($task);
            $this->maybeFinalize($node->id, $task->id);
            return $task;
        }

        $this->tasks->dispatchAfterCommit($task);

        return $task;
    }

    public function updateDesiredEnabled(int $nodeId, int $taskId, bool $shouldEnable): bool
    {
        return DB::transaction(function () use ($nodeId, $taskId, $shouldEnable) {
            $task = AsyncTask::query()->whereKey($taskId)->lockForUpdate()->first();
            $node = Node::query()->whereKey($nodeId)->lockForUpdate()->first();
            if ($task === null || $node === null || !in_array($task->status, [AsyncTask::STATUS_PENDING, AsyncTask::STATUS_RUNNING], true)) {
                return false;
            }

            $meta = $task->meta ?? [];
            $meta['should_enable'] = $shouldEnable;
            $task->update(['meta' => $meta]);
            $node->forceFill(['enabled' => false, 'status' => 'offline'])->save();

            return true;
        });
    }

    /**
     * item 终态后调用：全部 item 终态时定节点最终状态。
     * 任务行加锁读，防最后一项并发完成时漏判。
     * 空任务（completeEmpty 已置 succeeded 终态）也会进来定状态。
     */
    public function maybeFinalize(int $nodeId, int $taskId): void
    {
        DB::transaction(function () use ($nodeId, $taskId) {
            $task = AsyncTask::query()->whereKey($taskId)->lockForUpdate()->first();
            $node = Node::query()->whereKey($nodeId)->lockForUpdate()->first();
            if ($task === null || $node === null) {
                return;
            }

            $active = $task->items()->whereIn('status', [
                \App\Models\AsyncTaskItem::STATUS_PENDING,
                \App\Models\AsyncTaskItem::STATUS_RUNNING,
            ])->count();
            if ($active > 0) {
                return;
            }

            if ($task->status !== AsyncTask::STATUS_SUCCEEDED || $task->failed > 0) {
                // 初始化有失败：只标 offline 让订阅暂时跳过它，绝不改 enabled
                // （部分用户未建号是"半初始化"而非"节点不可用"，改 enabled 会连坐其余用户）。
                $node->forceFill(['status' => 'offline'])->save();
                return;
            }

            $node->forceFill([
                'enabled' => (bool) ($task->meta['should_enable'] ?? false),
                'status' => 'offline',
            ])->save();
        });
    }
}
