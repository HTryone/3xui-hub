<?php

namespace App\Jobs;

use App\Models\Node;
use App\Models\User;
use App\Services\AsyncTaskService;
use App\Services\NodeInboundSyncService;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;

/**
 * 节点入站变更 · 用户挂载（AsyncTask item，item_key=user:{id}）。
 *
 * 一个 Job 处理 1~2 个用户（批大小 config('tasks.node_sync_batch')，默认 2，见该配置的注释）。
 * 批处理省掉的只是「每个 Job 的固定开销」（队列取任务、任务行加锁、连带的往返），
 * 每个用户该发的 attach 一次不少。
 *
 * 关键约束：**同批用户之间互不影响，可见性不打折**
 * - 每个用户单独 claimItem / completeItem / failItem，各自的 item 状态就是日志页看到的那一行；
 * - 某个用户抛异常时，先把异常记下、继续跑完同批的其它用户，最后再把第一个异常抛给队列。
 *   于是「一个成功一个失败」= 成功那个的 item 已是 succeeded（重试时 claimItem 会发现并跳过），
 *   失败那个的 item 留给队列重试；重试整批不会让已成功的用户重复劳动。
 * - 幂等：入站 id 不随 Job 传递，执行时按 node->inboundIdsFor(user.protocol) 现取，
 *   与「算差异、派发」之间若还夹着一次编辑，取最新的才是用户最终该挂的入站；
 *   attach 本身是幂等的（同一批 id 重复挂结果不变），重放不产生副作用。
 *
 * 队列：读 config('panel.node_ops_queue')，默认 'default'（与引入开关前一致）。
 */
class NodeInboundSyncJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public int $tries = 3;

    public array $backoff = [30, 120, 300];

    /**
     * @param list<int>    $userIds  本批要处理的用户（1~2 个，见 config('tasks.node_sync_batch')）
     * @param list<string> $itemKeys 与 $userIds 一一对应的 item_key（user:{id}）
     */
    public function __construct(
        public int $nodeId,
        public array $userIds,
        public ?int $taskId = null,
        public array $itemKeys = [],
    ) {
        $this->onQueue(config('panel.node_ops_queue') ?: 'default');
    }

    public function handle(AsyncTaskService $tasks, NodeInboundSyncService $sync): void
    {
        /** @var Node|null $node */
        $node = Node::find($this->nodeId);

        /** @var \Illuminate\Support\Collection<int, User> $users */
        $users = User::query()->whereIn('id', $this->userIds)->get()->keyBy('id');

        $firstError = null;

        foreach (array_values($this->userIds) as $index => $userId) {
            $itemKey = $this->itemKeys[$index] ?? null;

            if ($this->taskId !== null && $itemKey !== null && !$tasks->claimItem($this->taskId, $itemKey)) {
                continue; // 已被处理（幂等重放 / 同批里已成功过）
            }

            /** @var User|null $user */
            $user = $users->get($userId);

            // 节点已删 / 用户已删 / 用户已失去套餐：该项无事可做，判成功（不影响同批其余用户）
            if ($node === null || $user === null || $user->plan_id === null) {
                $this->complete($tasks, $itemKey);

                continue;
            }

            try {
                $sync->syncUserOnNode($node, $user, $node->inboundIdsFor($user->protocol));
                $this->complete($tasks, $itemKey);
            } catch (\Throwable $e) {
                // 记下第一个异常，继续处理同批的下一个用户 —— 一个用户失败不该拖住另一个。
                // 该 item 停在 running，交给队列重试；重试用尽后由 failed() 标失败。
                $firstError ??= $e;
            }
        }

        if ($firstError !== null) {
            throw $firstError;
        }
    }

    public function failed(\Throwable $exception): void
    {
        if ($this->taskId === null) {
            return;
        }

        $tasks = app(AsyncTaskService::class);

        // 整批重试用尽：把本批里还没进终态的 item 全部标失败。
        // 已经 succeeded 的 item 会被 failItem 内部跳过（finishItem 对成功项直接返回），
        // 所以「一个成功一个失败」不会因为这一刀把成功的那个也染成失败。
        foreach ($this->itemKeys as $itemKey) {
            $tasks->failItem(
                $this->taskId,
                (string) $itemKey,
                "用户入站同步失败：{$exception->getMessage()}"
            );
        }
    }

    private function complete(AsyncTaskService $tasks, ?string $itemKey): void
    {
        if ($this->taskId !== null && $itemKey !== null) {
            $tasks->completeItem($this->taskId, $itemKey);
        }
    }
}
