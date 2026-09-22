<?php

namespace App\Jobs;

use App\Models\Node;
use App\Models\User;
use App\Services\UserAdminService;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldBeUnique;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;

/**
 * 异步建号：一台节点一个 Job（用户 × 节点）。
 *
 * 背景：同步版 UserAdminService::provisionClient 会把「N 个节点 × M 个入站」的串行
 * HTTPS 请求全压在 HTTP 请求里，慢节点吃满 connect_timeout 后注册要等 5~15 秒。
 * 拆成按节点派发后，注册接口只负责派发，立刻返回 token。
 *
 * 单节点逻辑不在这里重写，直接调用 UserAdminService::provisionClientOnNode，
 * 与同步路径共用同一段实现（uuid 回写、per-inbound try/catch 等全部保留）。
 *
 * 【防重】ShouldBeUnique：同一「用户 × 节点」在锁被持有期间只允许一个 Job 在队列或执行中，
 * 避免「注册 + 购买」并发时对同一 email 重复 createClient。
 * - 锁在 Job 终态（成功/失败）释放，uniqueFor 兜底防止 worker 崩溃后锁被永久占用。
 * - 唯一锁只能约束【走队列的派发方】，同步调用 provisionClient 的 5 个入口不在保护范围内；
 *   那几处靠 provisionClientOnNode 内部「先 getClient 再 create」的幂等兜底。
 *
 * 【重试】tries=3：Job 内部对节点/入站的失败已经 report() 吞掉（与同步版语义一致），
 * 真正的失败只可能来自 DB 抖动或驱动工厂异常，重试是安全的（同一 email 重复写是幂等的）。
 */
class ProvisionClientJob implements ShouldQueue, ShouldBeUnique
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public int $tries = 3;

    public array $backoff = [30, 120, 300];

    public int $uniqueFor = 600;

    public function __construct(
        public int $userId,
        public int $nodeId,
    ) {}

    /** 唯一锁粒度：用户 × 节点。 */
    public function uniqueId(): string
    {
        return "user:{$this->userId}:node:{$this->nodeId}";
    }

    public function handle(UserAdminService $users): void
    {
        /** @var User|null $user */
        $user = User::find($this->userId);
        if ($user === null) {
            return; // 用户已删：该节点无事可做
        }

        /** @var Node|null $node */
        $node = Node::find($this->nodeId);
        if ($node === null || !$node->enabled) {
            // 节点已删/已禁用：等价于同步遍历时的 enabled 过滤（派发到执行之间节点可能被关掉）
            return;
        }

        // 执行时重算负载：注册到 Job 真正开跑之间用户可能已购套餐，
        // 必须按最新数据写面板，否则会把刚开通的 client 关回去。
        $clientData = $users->prepareClientData($user);

        $users->provisionClientOnNode($user, $node, $clientData);
    }
}
