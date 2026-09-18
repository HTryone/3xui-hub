<?php

namespace App\Jobs;

use App\Models\Node;
use App\Models\User;
use App\Services\AsyncTaskService;
use App\Services\NodeInitService;
use RuntimeException;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;

/**
 * 节点接入 · 单用户初始化（P1-1/P1-2）。
 *
 * 节点接入时不再同步逐用户跑，改为按用户逐条派发本 Job（AsyncTask item）：
 * - 单用户失败：队列重试 3 次，最终只标"该项"失败，其余用户不受影响
 * - 每个 item 终态后触发 NodeInitService::maybeFinalize，全部完成才定节点启用
 * 幂等：先查该用户是否已存在→在则先删再建，可安全重放/重试。
 */
class NodeInitUserJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public int $tries = 3;
    public array $backoff = [30, 120, 300];

    public function __construct(
        public int $nodeId,
        public int $userId,
        public ?int $taskId = null,
        public ?string $itemKey = null,
    ) {}

    public function handle(
        AsyncTaskService $tasks,
        NodeInitService $init,
    ): void {
        if ($this->taskId !== null && $this->itemKey !== null) {
            if (!$tasks->claimItem($this->taskId, $this->itemKey)) {
                return; // 已被处理（幂等重放）
            }
        }

        /** @var Node|null $node */
        $node = Node::find($this->nodeId);
        if ($node === null) {
            if ($this->taskId !== null) {
                $tasks->failItem($this->taskId, $this->itemKey ?? '', '节点已删除，初始化中止');
            }
            return;
        }

        /** @var User|null $user */
        $user = User::find($this->userId);
        if ($user === null) {
            // 用户已删：该项无事可做，判成功（不影响节点定状态）
            if ($this->taskId !== null) {
                $tasks->completeItem($this->taskId, $this->itemKey ?? '');
            }
            $this->finalize($tasks, $init);
            return;
        }

        $driver = app(\App\Drivers\NodeDriverFactory::class)->make($node);
        $email = $user->clientEmail();
        $inboundIds = $node->inboundIdsFor($user->protocol);

        try {
            // 幂等清理：该用户在此节点已有残留则先删，再重建（避免重接后重复计流量）
            if ($driver->getClient($email) !== null) {
                $driver->deleteClient($email, false);
            }

            if ($user->plan_id === null || empty($inboundIds)) {
                // 无套餐或未配置入站：无 client 可建，仅清理残留即可
                if ($this->taskId !== null) {
                    $tasks->completeItem($this->taskId, $this->itemKey ?? '');
                }
                $this->finalize($tasks, $init);
                return;
            }

            $created = $driver->createClient([
                'email' => $email,
                'enable' => (bool) $user->enabled,
                'totalGB' => $user->traffic_limit > 0 ? (int) $user->traffic_limit : 0,
                'expiryTime' => $user->expired_at ? (int) ($user->expired_at->timestamp * 1000) : 0,
                'limitIp' => 0,
            ], $inboundIds);

            if ($created === null) {
                throw new RuntimeException("用户 {$email} 创建失败");
            }

            if ($this->taskId !== null) {
                $tasks->completeItem($this->taskId, $this->itemKey ?? '');
            }
        } finally {
            // 成功/失败都推进 finalize（失败时 completeItem 未调用，item 由 failed() 置 failed）
            $this->finalize($tasks, $init);
        }
    }

    public function failed(\Throwable $exception): void
    {
        if ($this->taskId === null) {
            return;
        }
        $tasks = app(AsyncTaskService::class);
        $tasks->failItem($this->taskId, $this->itemKey ?? '', "用户初始化失败：{$exception->getMessage()}");
        app(NodeInitService::class)->maybeFinalize($this->nodeId, $this->taskId);
    }

    private function finalize(AsyncTaskService $tasks, NodeInitService $init): void
    {
        if ($this->taskId !== null) {
            $init->maybeFinalize($this->nodeId, $this->taskId);
        }
    }
}
