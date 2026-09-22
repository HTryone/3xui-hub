<?php

namespace App\Jobs;

use App\Drivers\Contracts\PanelDriverInterface;
use App\Drivers\NodeDriverFactory;
use App\Models\Node;
use App\Models\User;
use App\Services\AsyncTaskService;
use App\Services\BanService;
use App\Services\NodeInitService;
use RuntimeException;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;

/**
 * 节点接入 · 用户初始化（P1-1/P1-2，AsyncTask item_key=user:{id}）。
 *
 * 节点接入时不再同步逐用户跑，改为按用户逐条派发本 Job（AsyncTask item）：
 * - 一个 Job 处理 1~2 个用户（批大小 config('tasks.node_sync_batch')，理由见该配置的注释）
 * - 单用户失败：队列重试 3 次，最终只标"该项"失败，其余用户不受影响
 * - 每个 item 终态后触发 NodeInitService::maybeFinalize，全部完成才定节点启用
 * 幂等：先查该用户是否已存在→在则先删再建，可安全重放/重试。
 *
 * 批处理省掉的只是「每个 Job 的固定开销」（队列取任务、任务行加锁、驱动构造），
 * 每个用户该做的清理 + 建号一次不少；item 粒度不变，日志页仍是逐用户一行、失败可单条重试。
 *
 * 关键约束：**同批用户之间互不影响，可见性不打折**
 * - 每个用户单独 claimItem / completeItem / failItem，各自的 item 状态就是日志页看到的那一行；
 * - 某个用户抛异常时，先把异常记下、继续跑完同批的其它用户，最后再把第一个异常抛给队列。
 *   于是「一个成功一个失败」= 成功那个的 item 已是 succeeded（重试时 claimItem 会发现并跳过），
 *   失败那个的 item 留给队列重试；重试整批不会让已成功的用户重复劳动。
 *
 * finalize 时机：**本批跑完（含异常路径）定一次**，而不是每个用户各定一次。
 * maybeFinalize 是「任务的全部 item 是否都进终态」的全局判断，逐用户调用只是把同一个全局判断
 * 多做 N 次（每次还要锁一次任务行 + 节点行）；批末尾调一次结果完全相同，节点行锁竞争却减半。
 * 既不会提前判：它按任务行加锁现算 active，同批后一个 item 还在 pending/running 时直接返回；
 * 也不会漏判：最后一批的末尾必然跑在全部 item 终态之后，异常路径上未终态的 item 留给 failed()
 * 兜底（那里同样会调 maybeFinalize）。
 *
 * 队列：读 config('panel.node_ops_queue')，默认 'default'（与引入开关前一致）。
 */
class NodeInitUserJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public int $tries = 3;
    public array $backoff = [30, 120, 300];

    /**
     * @param list<int>    $userIds  本批要初始化的用户（1~2 个，见 config('tasks.node_sync_batch')）
     * @param list<string> $itemKeys 与 $userIds 一一对应的 item_key（user:{id}）
     */
    public function __construct(
        public int $nodeId,
        public array $userIds,
        public ?int $taskId = null,
        public array $itemKeys = [],
    ) {
        // 节点类 Job 走同一条可配置队列；默认 'default'，只有部署侧显式设了
        // PANEL_NODE_OPS_QUEUE 并起了对应 worker 才切走（理由见 config/panel.php）
        $this->onQueue(config('panel.node_ops_queue') ?: 'default');
    }

    public function handle(
        AsyncTaskService $tasks,
        NodeInitService $init,
    ): void {
        /** @var Node|null $node */
        $node = Node::find($this->nodeId);

        /** @var \Illuminate\Support\Collection<int, User> $users */
        $users = User::query()->whereIn('id', $this->userIds)->get()->keyBy('id');

        // 驱动按批构造一次：它本身不带用户态（email 逐次传入），同批复用省掉重复构造
        // 以及随连接走的登录态还原（cookie 模式的跨进程缓存命中）。
        $driver = $node !== null ? app(NodeDriverFactory::class)->make($node) : null;

        $firstError = null;

        try {
            foreach (array_values($this->userIds) as $index => $userId) {
                $itemKey = $this->itemKeys[$index] ?? null;

                // 每个用户单独认领：重试整批时上一轮已成功的 item 在这里被跳过，不重复清理/建号
                if ($this->taskId !== null && $itemKey !== null && !$tasks->claimItem($this->taskId, $itemKey)) {
                    continue;
                }

                if ($node === null || $driver === null) {
                    // 节点已删：本批每一项各自判失败（照旧逐项可见），不牵连同批其它项
                    if ($this->taskId !== null && $itemKey !== null) {
                        $tasks->failItem($this->taskId, $itemKey, '节点已删除，初始化中止');
                    }

                    continue;
                }

                /** @var User|null $user */
                $user = $users->get($userId);
                if ($user === null) {
                    // 用户已删：该项无事可做，判成功（不影响节点定状态）
                    $this->complete($tasks, $itemKey);

                    continue;
                }

                try {
                    $this->initUser($driver, $node, $user);
                    $this->complete($tasks, $itemKey);
                } catch (\Throwable $e) {
                    // 记下第一个异常，继续处理同批的下一个用户 —— 一个用户失败不该拖住另一个。
                    // 该 item 停在 running，交给队列重试；重试用尽后由 failed() 标失败。
                    $firstError ??= $e;
                }
            }
        } finally {
            // 本批末尾定一次节点终态。异常路径同样要定：同批可能已有 item 进入终态，
            // 而正在重试的那个尚未终态（maybeFinalize 会因 active>0 自己返回）。
            $this->finalize($tasks, $init);
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
                "用户初始化失败：{$exception->getMessage()}"
            );
        }

        app(NodeInitService::class)->maybeFinalize($this->nodeId, $this->taskId);
    }

    /**
     * 单个用户在这台节点上的初始化（含幂等清理）。
     * 失败时抛异常，由 handle 决定「记下并继续同批」。
     */
    private function initUser(PanelDriverInterface $driver, Node $node, User $user): void
    {
        $email = $user->clientEmail();
        $inboundIds = $node->inboundIdsFor($user->protocol);

        // 幂等清理：该用户在此节点已有残留则先删，再重建（避免重接后重复计流量）
        if ($driver->getClient($email) !== null) {
            $driver->deleteClient($email, false);
        }

        if ($user->plan_id === null || empty($inboundIds)) {
            // 无套餐或未配置入站：无 client 可建，仅清理残留即可
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

        // 该路径以 enable=true 建 client（等价于把用户重新打开）：撤销「已关闭」标记，
        // 否则扫描器会以为用户仍关闭而跳过校验（最长 config('ban.recheck_after_hours')）。
        if ($user->enabled) {
            app(BanService::class)->forgetDisabledState($user);
        }
    }

    private function complete(AsyncTaskService $tasks, ?string $itemKey): void
    {
        if ($this->taskId !== null && $itemKey !== null) {
            $tasks->completeItem($this->taskId, $itemKey);
        }
    }

    private function finalize(AsyncTaskService $tasks, NodeInitService $init): void
    {
        if ($this->taskId !== null) {
            $init->maybeFinalize($this->nodeId, $this->taskId);
        }
    }
}
