<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\Order;
use App\Models\ProxyAccount;
use App\Models\ProxyEntitlement;
use App\Models\ProxySocks;
use App\Services\ProxyStoreService;
use App\Services\Webshare\ProxyProviderService;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;

/**
 * 代理运营：把「谁买了代理、还剩多少、什么时候到期、该不该停」集中到一页。
 *
 * 分成三类数据，对应真实业务：
 *   - 住宅账号（webshare 独占）：一个用户一个上游子账号，要能回采 / 停用 / 重置密码 / 延期 / 删除
 *   - 共享 socks：一条线路多人共用，要能看「谁在用这条线」，以及在线路坏掉时整条迁移
 *   - 待补开：钱已付、但上游开号失败没发出权益的订单，必须能一眼看到并重试
 *
 * 说明：这里只读写商城自己的数据（proxy_accounts / proxy_entitlements / orders），
 * 不会碰节点套餐与订阅。
 */
class ProxyOpsController extends Controller
{
    use ApiResponse;

    public function __construct(
        private ProxyStoreService $store,
        private ProxyProviderService $proxyAccounts,
    ) {}

    /**
     * 运营总览：只出统计 + socks 线路下拉（轻量，30 秒刷新用）。
     *
     * 三个列表各自分页走下面的独立接口 —— 一个接口塞三张分页表，
     * 翻账号页时待补开也会跟着跳页，交互会乱。
     */
    public function overview(): JsonResponse
    {
        $now = now();

        $activeQuery = ProxyEntitlement::query()
            ->where('status', ProxyEntitlement::STATUS_ACTIVE)
            ->where(fn ($q) => $q->whereNull('expires_at')->orWhere('expires_at', '>', $now));

        $stats = [
            'active' => (clone $activeQuery)->count(),
            'expiring' => (clone $activeQuery)->whereNotNull('expires_at')
                ->where('expires_at', '<=', $now->copy()->addDays(7))->count(),
            'expired' => ProxyEntitlement::query()->where(function ($q) use ($now) {
                $q->where('status', ProxyEntitlement::STATUS_EXPIRED)
                    ->orWhere(fn ($w) => $w->where('status', ProxyEntitlement::STATUS_ACTIVE)
                        ->whereNotNull('expires_at')->where('expires_at', '<=', $now));
            })->count(),
            'pending_orders' => $this->pendingOrderQuery()->count(),
            'accounts' => ProxyAccount::query()->whereNotNull('sku_id')->count(),
            'socks_lines' => ProxyEntitlement::query()->where('kind', ProxyEntitlement::KIND_SOCKS)
                ->distinct()->count('socks_id'),
        ];

        return $this->success([
            'stats' => $stats,
            'socks_options' => ProxySocks::query()->where('is_active', true)
                ->orderByDesc('id')
                ->get(['id', 'host', 'port', 'country'])
                ->map(fn (ProxySocks $s) => [
                    'id' => $s->id,
                    'label' => $s->host . ':' . $s->port . ($s->country ? ' · ' . $s->country : ''),
                ])
                ->values(),
        ]);
    }

    /**
     * 住宅账号（分页）。
     *
     * 只列商城卖出的（有 sku_id）；管理员在 Webshare 页手动开的号归那边管。
     * 支持 status（active/disabled/expired…）与 search（邮箱 / 用户 ID / 账号标识）。
     */
    public function accounts(Request $request): JsonResponse
    {
        $query = ProxyAccount::query()
            ->with(['provider', 'user', 'sku', 'latestEntitlement'])
            ->whereNotNull('sku_id')
            ->orderByRaw('expires_at IS NULL, expires_at ASC')
            ->orderByDesc('id');

        $status = trim((string) $request->query('status', ''));
        if ($status !== '') {
            $query->where('status', $status);
        }

        $search = trim((string) $request->query('search', ''));
        if ($search !== '') {
            $like = '%' . $search . '%';
            $query->where(function ($q) use ($search, $like) {
                $q->where('label', 'like', $like)
                    ->orWhere('username', 'like', $like);
                if (ctype_digit($search)) {
                    $q->orWhere('user_id', (int) $search);
                }
                $q->orWhereHas('user', fn ($u) => $u->where('email', 'like', $like));
            });
        }

        $perPage = min(100, max(10, (int) $request->query('per_page', 50)));

        return $this->successPage($query, function (ProxyAccount $a) {
            // latestEntitlement 已预加载（见 ProxyAccount 关联），不再逐行查库
            $entitlement = $a->latestEntitlement;

            return [
                'id' => $a->id,
                'entitlement_id' => $entitlement?->id,
                'order_id' => $a->order_id,
                'label' => $a->label,
                'username' => $a->username,
                'user' => $a->user ? ['id' => $a->user->id, 'email' => $a->user->email] : null,
                'provider_name' => $a->provider?->name,
                'sku_name' => $a->sku?->name,
                'proxy_limit' => (float) $a->proxy_limit,
                'used_bytes' => (int) $a->used_bytes,
                'limit_bytes' => $a->traffic_limit_bytes ? (int) $a->traffic_limit_bytes : null,
                'status' => $a->status,
                'expires_at' => $a->expires_at?->toIso8601String(),
                'last_synced_at' => $a->last_synced_at?->toIso8601String(),
            ];
        }, $perPage);
    }

    /**
     * 共享 socks 线路（分页，按线路聚合）。
     *
     * 分页对象是「线路」不是「权益」：一页 20 条线路，每条带当前在用的用户明细。
     * 先按 socks_id 去重分页取当前页的线路，再批量查这些线路的权益，避免全表扫描。
     */
    public function socksGroups(Request $request): JsonResponse
    {
        $perPage = min(50, max(5, (int) $request->query('per_page', 20)));
        $page = max(1, (int) $request->query('page', 1));

        $base = ProxyEntitlement::query()->where('kind', ProxyEntitlement::KIND_SOCKS);
        $totalLines = (clone $base)->distinct()->count('socks_id');
        $lastPage = max(1, (int) ceil($totalLines / $perPage));
        $page = min($page, $lastPage);

        $socksIds = (clone $base)
            ->select('socks_id')
            ->distinct()
            ->orderByDesc('socks_id')
            ->forPage($page, $perPage)
            ->pluck('socks_id');

        $items = collect();
        if ($socksIds->isNotEmpty()) {
            $ents = ProxyEntitlement::query()
                ->with(['socks', 'user', 'sku'])
                ->where('kind', ProxyEntitlement::KIND_SOCKS)
                ->whereIn('socks_id', $socksIds)
                ->orderByDesc('id')
                ->get()
                ->groupBy('socks_id');

            $items = $socksIds->map(function ($socksId) use ($ents) {
                $group = $ents->get($socksId, collect());
                $first = $group->first();
                $sock = $first?->socks;

                $activeUsers = $group->filter(fn ($e) => $e->isActive());
                $nextExpiry = $activeUsers->filter(fn ($e) => $e->expires_at)
                    ->min(fn ($e) => $e->expires_at->timestamp);

                return [
                    'socks_id' => $socksId,
                    'host' => $sock?->host,
                    'port' => $sock?->port,
                    'country' => $sock?->country,
                    'socks_active' => (bool) ($sock?->is_active ?? false),
                    'user_count' => $activeUsers->count(),
                    'total_count' => $group->count(),
                    'next_expiry' => $nextExpiry ? date(DATE_ATOM, $nextExpiry) : null,
                    'users' => $group->map(fn (ProxyEntitlement $e) => [
                        'entitlement_id' => $e->id,
                        'user_id' => $e->user_id,
                        'email' => $e->user?->email,
                        'sku_name' => $e->sku?->name,
                        'status' => $e->isActive() ? 'active' : $e->status,
                        'expires_at' => $e->expires_at?->toIso8601String(),
                    ])->values(),
                ];
            })->values();
        }

        return response()->json([
            'code' => 0,
            'msg' => 'ok',
            'data' => $items,
            'pagination' => [
                'current_page' => $page,
                'last_page' => $lastPage,
                'total' => $totalLines,
                'per_page' => $perPage,
            ],
        ]);
    }

    /** 待补开订单（分页）：已支付但没发出权益的代理单 */
    public function pending(Request $request): JsonResponse
    {
        $perPage = min(100, max(10, (int) $request->query('per_page', 50)));

        return $this->successPage(
            $this->pendingOrderQuery()->with(['user', 'proxySku'])->orderByDesc('id'),
            fn (Order $o) => [
                'id' => $o->id,
                'order_no' => $o->order_no,
                'user' => $o->user ? ['id' => $o->user->id, 'email' => $o->user->email] : null,
                'sku_name' => $o->proxySku?->name,
                'amount' => (float) $o->amount,
                'paid_at' => $o->paid_at?->toIso8601String(),
                'renewal' => (bool) $o->proxy_renewal,
            ],
            $perPage,
        );
    }

    /** 回采用量（单个账号） */
    public function pull(ProxyAccount $account): JsonResponse
    {
        try {
            $used = $this->proxyAccounts->pullUsage($account);
        } catch (\Throwable $e) {
            return $this->error('回采失败：' . $e->getMessage(), 502);
        }

        return $this->success(['used_bytes' => $used], '已回采');
    }

    /** 停用账号（改随机密码 + 额度压到 0.01GB） */
    public function disable(ProxyAccount $account): JsonResponse
    {
        try {
            $this->proxyAccounts->disableAccount($account);
        } catch (\Throwable $e) {
            return $this->error('停用失败：' . $e->getMessage(), 502);
        }

        return $this->success(null, '已停用');
    }

    /**
     * 重置密码（客服交付场景）。
     *
     * 返回的明文只在这里出现一次，库里存密文；同时把额度恢复成权益规格的额度，
     * 这样「停用过的号」也能靠这一步直接恢复可用。
     */
    public function resetPassword(ProxyAccount $account): JsonResponse
    {
        $entitlement = ProxyEntitlement::query()
            ->where('account_id', $account->id)
            ->orderByDesc('id')
            ->first();

        $sku = $entitlement?->sku;

        try {
            $this->proxyAccounts->renewAccount($account, [
                'proxy_limit' => (float) ($sku?->traffic_gb ?: $account->proxy_limit),
                'max_thread_count' => (int) ($sku?->max_thread_count ?: $account->max_thread_count),
            ]);
        } catch (\Throwable $e) {
            return $this->error('重置失败：' . $e->getMessage(), 502);
        }

        return $this->success([
            'username' => $account->username,
            'password' => $account->password,
        ], '已重置，请把新凭据发给用户');
    }

    /** 延长权益（补偿 / 客服），可选同时恢复账号 */
    public function extend(Request $request, ProxyEntitlement $entitlement): JsonResponse
    {
        $data = $request->validate(['days' => ['required', 'integer', 'min:1', 'max:365']]);

        $this->store->extendEntitlement($entitlement, (int) $data['days']);

        return $this->success([
            'id' => $entitlement->id,
            'expires_at' => $entitlement->fresh()->expires_at?->toIso8601String(),
        ], "已延长 {$data['days']} 天");
    }

    /** 删除账号（上游 + 本地） */
    public function destroy(ProxyAccount $account): JsonResponse
    {
        try {
            $this->proxyAccounts->deleteAccount($account);
        } catch (\Throwable $e) {
            return $this->error('删除失败：' . $e->getMessage(), 502);
        }

        return $this->success(null, '已删除');
    }

    /** 某个住宅账号可用的出口 IP */
    public function proxies(ProxyAccount $account): JsonResponse
    {
        if (! $account->provider) {
            return $this->error('账号缺少上游配置', 400);
        }

        try {
            $result = $this->proxyAccounts->listProxies($account->provider);
        } catch (\Throwable $e) {
            return $this->error('获取 IP 失败：' . $e->getMessage(), 502);
        }

        return $this->success($result['items']);
    }

    /** 重试开通（钱已付但上游开号失败的订单） */
    public function retryOrder(Order $order): JsonResponse
    {
        if (! $order->isProxyOrder()) {
            return $this->error('不是代理订单', 400);
        }
        if ($order->status !== Order::STATUS_PAID) {
            return $this->error('只有已支付的订单能重试开通', 400);
        }

        $this->store->completeOrder($order);

        $created = ProxyEntitlement::where('order_id', $order->id)->exists();
        if (! $created) {
            return $this->error('仍然开通失败，请查看日志或检查上游额度', 502);
        }

        return $this->success(null, '已开通');
    }

    /** 把一条共享 socks 权益换到另一条线路 */
    public function moveSocks(Request $request, ProxyEntitlement $entitlement): JsonResponse
    {
        $data = $request->validate(['socks_id' => ['required', 'integer']]);

        try {
            $this->store->moveSocksEntitlement($entitlement, (int) $data['socks_id']);
        } catch (\InvalidArgumentException $e) {
            return $this->error($e->getMessage(), 400);
        }

        return $this->success(null, '已换线路');
    }

    /** 已支付但没有发出权益的代理订单 */
    private function pendingOrderQuery()
    {
        return Order::query()
            ->where('order_type', Order::TYPE_PROXY)
            ->where('status', Order::STATUS_PAID)
            ->whereNotIn('id', ProxyEntitlement::query()->whereNotNull('order_id')->select('order_id'));
    }
}
