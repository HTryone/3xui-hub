<?php

namespace App\Services;

use App\Drivers\NodeDriverFactory;
use App\Jobs\NodeInboundScanJob;
use App\Models\AsyncTask;
use App\Models\AsyncTaskItem;
use App\Models\Node;
use App\Models\User;
use Illuminate\Support\Facades\DB;

/**
 * 节点入站变更后的「把用户挂到新入站」同步（异步、按用户粒度）。
 *
 * 背景：保存节点入站时，最初实现在 HTTP 请求里对每个有套餐用户发 getClient + attachClient
 * 两次面板请求（100 用户 = 200 次串行 HTTPS），前端 15 秒超时闪断、浏览器早退而请求还在跑。
 * 第一版异步化把面板请求挪进队列，但仍是**逐用户**派 Job、每个 Job 再 getClient 问一次面板
 * 「你在不在」—— 100 用户依旧是 100 次「问」+ 100 次「挂」。
 *
 * 现在改成两段：
 * 1. submit()：建任务（item 仍是全部相关用户）+ 派发【一台节点一个】的扫描 Job，
 *    请求内依旧零面板请求；
 * 2. scanNode()：先拉【一次】clients/list 拿到该节点全部客户端及其所属入站，与
 *    「应该在这个节点上的用户」做 diff —— 已经挂好的直接 completeItem（连面板请求都不用发），
 *    只有缺挂载的才 dispatch() 出执行 Job。
 *    100 用户、实际只有 3 个需要挂时：面板请求 = 1（列表）+ 3（attach）。
 *
 * 单用户执行逻辑集中在 syncUserOnNode()。
 */
class NodeInboundSyncService
{
    public function __construct(
        private AsyncTaskService $tasks,
        private NodeDriverFactory $driverFactory,
    ) {
    }

    /**
     * 按协议算出需要同步的用户并派发任务。
     *
     * 与旧 syncUsersToInbounds 的选人条件一致：只取「入站集合确实变了」的协议下、有套餐的用户
     * （无套餐用户面板上本来就不该有 client，无需挂入站）。
     *
     * item 仍然是【全部】相关用户（日志页看得到总量与逐人进度），但执行 Job 只发给扫描判定
     * 需要动手的那几个 —— 派发扫描 Job 这件事本身不发任何面板请求。
     *
     * @param array<string, list<int>> $changedProtocols 协议 => 该协议的新入站 id 列表
     * @return AsyncTask|null 没有用户需要同步时返回 null（不派发空任务）
     */
    public function submit(Node $node, array $changedProtocols): ?AsyncTask
    {
        $userIds = User::query()
            ->whereIn('protocol', array_keys($changedProtocols))
            ->whereNotNull('plan_id')
            ->pluck('id')
            ->all();

        if ($userIds === []) {
            return null;
        }

        $task = $this->tasks->create(
            'node_action',
            'node',
            $node->id,
            count($userIds),
            [
                'action' => 'sync_inbounds',
                'node_name' => $node->name,
                'inbounds' => $changedProtocols,
            ],
            3,
            array_map(fn (int $id) => "user:{$id}", $userIds),
        );

        // 只派扫描：不是 dispatchAfterCommit($task)，那会把全部 item 逐个派出去。
        DB::afterCommit(fn () => NodeInboundScanJob::dispatch($node->id, $task->id));

        return $task;
    }

    /**
     * 扫描单个节点：拉一次客户端清单，与任务里待处理的 item 做 diff。
     *
     * 判定口径（与「扫描前」逐用户 getClient 的语义对齐）：
     * - 面板上**没有**这个 client   → completeItem。老代码 getClient 返回 null 就是「跳过」，
     *   这里一比一沿用：不派 Job、不发请求。ControlHub 只在接入初始化时建 client，
     *   入站同步不负责建号，所以「没 client」是常态而非异常。
     * - 面板上有、且**已挂**全部目标入站 → completeItem（零面板请求，这是本次提速的主要来源）。
     * - 面板上有、但**没挂全**目标入站 → 交给执行 Job 去 attach。
     * - 列表项里没有 inboundIds 字段（老版本面板拿不到挂载信息）→ 一律当「没挂全」交给 Job，
     *   宁可多发一次 attach（幂等）也不要漏挂。
     *
     * 清单拉不到（节点不可达 / 鉴权失败 / 响应非法）：把**全部**待处理 item 标失败并写明原因，
     * 任务整体判失败。这里【不】靠队列重试：重试窗口（3 次 ≈ 7.5 分钟）大概率等不到节点恢复，
     * 却会让任务挂在 running 上，反而可能先被 async-task-timeout 判失败。管理员看到明确原因、
     * 修好后手点重试即可（重试会重新走一遍本方法，见 AsyncTaskService::retry）。
     */
    public function scanNode(Node $node, AsyncTask $task): void
    {
        try {
            $driver = $this->driverFactory->make($node);
            $clients = $driver->listClients();
        } catch (\Throwable $e) {
            $this->tasks->failPendingItems($task, '拉取节点客户端清单失败，未能同步入站：' . $e->getMessage());

            return;
        }

        // email => 已在哪些入站上；null 表示面板没给出挂载信息（无法判定，按「需要处理」走）
        $mounted = [];
        foreach ($clients as $client) {
            $email = $client['email'] ?? null;
            if (!is_string($email) || $email === '') {
                continue;
            }
            $inboundIds = $client['inboundIds'] ?? null;
            $mounted[$email] = is_array($inboundIds)
                ? array_map('intval', $inboundIds)
                : null;
        }

        $pendingKeys = $task->items()
            ->where('status', AsyncTaskItem::STATUS_PENDING)
            ->pluck('item_key')
            ->all();

        if ($pendingKeys === []) {
            return;
        }

        $userIds = [];
        foreach ($pendingKeys as $itemKey) {
            [, $id] = array_pad(explode(':', (string) $itemKey, 2), 2, null);
            $userIds[] = (int) $id;
        }

        /** @var \Illuminate\Support\Collection<int, User> $users */
        $users = User::query()->whereIn('id', $userIds)->get()->keyBy('id');

        // 入站按协议一次性载入，避免逐用户查库（100 用户 = 100 次查询）
        $inboundsByProtocol = $node->inbounds()
            ->get()
            ->groupBy('protocol')
            ->map(fn ($items) => $items->pluck('inbound_id')->map(fn ($id) => (int) $id)->all());

        $needsWork = [];
        foreach ($pendingKeys as $itemKey) {
            [, $id] = array_pad(explode(':', (string) $itemKey, 2), 2, null);
            /** @var User|null $user */
            $user = $users->get((int) $id);

            // 用户已删 / 已失去套餐：无事可做（与执行 Job 里的判断一致）
            if ($user === null || $user->plan_id === null) {
                $this->tasks->completeItem($task->id, (string) $itemKey);

                continue;
            }

            $inboundIds = $inboundsByProtocol->get($user->protocol, []);
            if ($inboundIds === []) {
                $this->tasks->completeItem($task->id, (string) $itemKey);

                continue;
            }

            $email = $user->clientEmail();
            if (!array_key_exists($email, $mounted)) {
                // 面板上根本没有这个 client：老语义就是跳过，不发任何请求
                $this->tasks->completeItem($task->id, (string) $itemKey);

                continue;
            }

            $attached = $mounted[$email];
            if ($attached !== null && array_diff($inboundIds, $attached) === []) {
                // 已经挂好了：本次提速的关键分支，直接判完成，一次面板请求都不发
                $this->tasks->completeItem($task->id, (string) $itemKey);

                continue;
            }

            $needsWork[] = (string) $itemKey;
        }

        // 只把这些 item 派出去执行；一个都不需要时什么也不派，任务已由上面的 completeItem 判终态
        if ($needsWork !== []) {
            $this->tasks->dispatch($task, $needsWork);
        }
    }

    /**
     * 单用户在节点上的一次挂载。
     *
     * 调用前【必须】已经确认该 email 在面板上存在 —— 扫描阶段用 clients/list 一次性问过，
     * 这正是本方法不再自己 getClient 的原因（100 用户省掉 100 次往返）。
     *
     * 唯一的例外是走「重试」进来：那条路会先重跑一次扫描，结论同样是准确的。
     * 因此这里直接 attach：attach 是幂等的（同一批 inboundIds 重复挂结果不变），
     * 重复执行 / 队列重放都不会产生副作用。
     *
     * @return bool 真的发了 attach 请求返回 true，跳过（该协议没配入站）返回 false
     */
    public function syncUserOnNode(Node $node, User $user, array $inboundIds): bool
    {
        if ($inboundIds === []) {
            return false;
        }

        $driver = $this->driverFactory->make($node);
        $driver->attachClient($user->clientEmail(), $inboundIds);

        return true;
    }
}
