<?php

namespace App\Services;

use App\Models\Order;
use App\Models\PaymentConfig;
use App\Models\ProxyAccount;
use App\Models\ProxyEntitlement;
use App\Models\ProxyProvider;
use App\Models\ProxySku;
use App\Models\ProxySocks;
use App\Models\User;
use App\Services\Webshare\ProxyProviderService;
use App\Services\Webshare\WebshareException;
use Illuminate\Support\Facades\Cache;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Log;

/**
 * 代理商城：下单 / 支付成功开通 / 续费 / 到期维护。
 *
 * 口径（与站长确认过的产品规则）：
 *   - 流量不归平台管：webshare 建号时把 GB 传给上游 proxy_limit，平台只读回用量展示
 *   - webshare 独占：一人一个子账号；续费只改额度/到期，不新开号（省上游子账号额度）
 *   - socks 共享：多人共用同一条 socks，不新建任何账号，只挂权益
 *   - 库存：新开通占一份（reserve→consume），续费不占；开通失败要把库存还回去
 */
class ProxyStoreService
{
    public function __construct(
        private PaymentService $paymentService,
        private ProxyProviderService $proxyAccounts,
    ) {}

    /**
     * 创建代理订单并返回支付链接。
     *
     * @return array{order_no:string, amount:float, status:string, pay_url:?string}
     */
    public function createOrder(User $user, int $skuId, ?int $paymentConfigId = null): array
    {
        $sku = ProxySku::with('product')->find($skuId);
        if (! $sku) {
            throw new \InvalidArgumentException('代理套餐不存在');
        }
        if (! $sku->is_active || ! $sku->product?->is_active) {
            throw new \InvalidArgumentException('该代理套餐已下架');
        }

        // 同一用户 1 分钟内重复点同一规格：挡住（与节点套餐同口径）
        $recent = Order::query()
            ->where('user_id', $user->id)
            ->where('order_type', Order::TYPE_PROXY)
            ->where('proxy_sku_id', $sku->id)
            ->where('status', Order::STATUS_PENDING)
            ->where('created_at', '>=', now()->subMinute())
            ->first();
        if ($recent) {
            throw new \InvalidArgumentException('请勿频繁下单，请 1 分钟后再试');
        }

        // 已有同货源类型的有效权益 → 走续费（不占库存，不新开上游账号）
        $renewal = $this->findRenewableEntitlement($user, $sku);
        $isRenewal = $renewal !== null;

        if (! $isRenewal && $sku->availableStock() <= 0) {
            throw new \InvalidArgumentException('该代理套餐已售完');
        }

        if ($paymentConfigId) {
            $payment = PaymentConfig::where('id', $paymentConfigId)->where('enabled', true)->first();
        } else {
            $payment = PaymentConfig::where('enabled', true)->first();
        }
        if (! $payment) {
            throw new \InvalidArgumentException('暂无可用支付方式');
        }

        $order = DB::transaction(function () use ($user, $sku, $payment, $isRenewal) {
            // 新开通才预占库存；预占失败说明刚好被别人买走
            if (! $isRenewal && ! ProxySku::reserveStock($sku->id, 1)) {
                throw new \InvalidArgumentException('该代理套餐已售完');
            }

            return Order::create([
                'order_no' => Order::generateOrderNo(),
                'user_id' => $user->id,
                'order_type' => Order::TYPE_PROXY,
                'plan_id' => null,
                'proxy_sku_id' => $sku->id,
                'proxy_renewal' => $isRenewal,
                'amount' => (float) $sku->price,
                'original_amount' => (float) $sku->price,
                'discount_amount' => 0,
                'status' => Order::STATUS_PENDING,
                'payment_config_id' => $payment->id,
                'pay_ip' => request()->ip(),
            ]);
        });

        // 免费规格（价格 0）直接开通，与节点套餐一致
        if ((float) $sku->price <= 0) {
            $this->completeOrder($order);

            return [
                'order_no' => $order->order_no,
                'amount' => 0.0,
                'status' => 'paid',
                'pay_url' => null,
            ];
        }

        [$payUrl, $failMsg] = $this->paymentService->buildPayUrl($payment, $order);
        if ($payUrl === '') {
            // 出链接失败：把预占的库存还回去，别让用户卡着库存
            if (! $isRenewal) {
                ProxySku::releaseStock($sku->id, 1);
            }
            $order->forceFill(['status' => Order::STATUS_EXPIRED])->save();

            throw new \RuntimeException($failMsg !== '' ? "获取支付链接失败：{$failMsg}" : '获取支付链接失败，请稍后再试');
        }

        return [
            'order_no' => $order->order_no,
            'amount' => (float) $order->amount,
            'status' => 'pending',
            'pay_url' => $payUrl,
        ];
    }

    /**
     * 支付成功：开通/续费（幂等，重复回调直接返回）。
     *
     * 失败处理原则：钱已收，订单保持 paid；把库存还回去、记 error 日志，
     * 由管理员在代理账号页手动补开（不抛异常，避免网关反复重推）。
     */
    /**
     * 支付成功：开通/续费（幂等 + 互斥）。
     *
     * 为什么要锁：开号要打上游，最长 21 秒（连接 6s + 总 15s）。网关等不到响应会重推回调，
     * 两条回调同时进来各自查「有没有权益」都是「没有」，就会各开一个上游子账号 ——
     * 钱只收一次，却白占一个子账号，用户还可能拿到两套凭据。
     * 拿不到锁说明另一条回调正在处理，直接返回，那条会负责开通。
     */
    public function completeOrder(Order $order): void
    {
        $lock = Cache::lock('proxy:provision:' . $order->id, 120);

        if (! $lock->get()) {
            Log::info('代理订单开通已在处理中，跳过重复回调', ['order_no' => $order->order_no]);

            return;
        }

        try {
            $this->doCompleteOrder($order);
        } finally {
            $lock->release();
        }
    }

    private function doCompleteOrder(Order $order): void
    {
        $sku = $order->proxySku;
        $user = $order->user;

        if (! $sku || ! $user) {
            Log::error('代理订单开通失败：规格或用户缺失', ['order_no' => $order->order_no]);

            return;
        }

        // 幂等：同一订单只开通一次
        if (ProxyEntitlement::where('order_id', $order->id)->exists()) {
            return;
        }

        $existing = $this->findRenewableEntitlement($user, $sku, includeExpired: true);

        try {
            if ($sku->source_type === 'socks') {
                $sock = ProxySocks::find($sku->source_id);
                if (! $sock || ! $sock->is_active) {
                    throw new \RuntimeException('共享 socks 货源不可用');
                }
                $this->upsertEntitlement($user, $order, $sku, ProxyEntitlement::KIND_SOCKS, null, $sock->id, $existing);
            } else {
                $account = $existing?->account_id ? ProxyAccount::find($existing->account_id) : null;

                if ($account) {
                    // 续费：把额度重置回规格额度、恢复启用、延长到期
                    $this->renewWebshareAccount($account, $sku);
                } else {
                    $account = $this->openWebshareAccount($order, $user, $sku);
                }

                $this->upsertEntitlement($user, $order, $sku, ProxyEntitlement::KIND_WEBSHARE, $account->id, null, $existing);
            }
        } catch (\Throwable $e) {
            // 上游失败：把库存还回去（新开通才占过），订单保持 paid，留日志给管理员补开
            if (! $order->proxy_renewal && $sku->source_type === 'webshare') {
                ProxySku::releaseStock($sku->id, 1);
            }
            Log::error('代理订单开通失败（用户已付款，需人工补开）', [
                'order_no' => $order->order_no,
                'user_id' => $user->id,
                'sku_id' => $sku->id,
                'error' => $e->getMessage(),
            ]);

            return;
        }

        // 新开通成功：库存占用转已售（续费不占库存）
        if (! $order->proxy_renewal) {
            ProxySku::consumeStock($sku->id, 1);
        }
    }

    /**
     * 订单超时/取消：把预占的库存还回去（节点订单不涉及）。
     */
    public function releaseOrderStock(Order $order): void
    {
        if (! $order->isProxyOrder() || $order->proxy_renewal || ! $order->proxy_sku_id) {
            return;
        }

        ProxySku::releaseStock((int) $order->proxy_sku_id, 1);
    }

    /**
     * 定时维护：回采 webshare 用量、到期停用、过期权益标记。
     *
     * 流量超量不在这里处理 —— Webshare 自己会断（proxy_limit 是硬上限）。
     */
    public function maintain(): array
    {
        $stats = ['pulled' => 0, 'pull_failed' => 0, 'expired' => 0, 'disabled' => 0];

        // 1) 回采用量（只处理 webshare 独占账号；socks 共享型没有用量）
        foreach (ProxyAccount::query()->where('status', 'active')->whereNotNull('sku_id')->cursor() as $account) {
            try {
                $this->proxyAccounts->pullUsage($account);
                $stats['pulled']++;
            } catch (WebshareException $e) {
                $stats['pull_failed']++;
                Log::warning('代理用量回采失败', ['account_id' => $account->id, 'error' => $e->getMessage()]);
            }
        }

        // 2) 到期权益：标记 expired，并把对应的 webshare 子账号停用
        foreach (ProxyEntitlement::query()
            ->where('status', ProxyEntitlement::STATUS_ACTIVE)
            ->whereNotNull('expires_at')
            ->where('expires_at', '<=', now())
            ->cursor() as $entitlement) {
            $entitlement->forceFill(['status' => ProxyEntitlement::STATUS_EXPIRED])->save();
            $stats['expired']++;

            if ($entitlement->account_id) {
                $account = ProxyAccount::find($entitlement->account_id);
                if ($account && $account->status === 'active') {
                    try {
                        $this->proxyAccounts->disableAccount($account);
                        $stats['disabled']++;
                    } catch (\Throwable $e) {
                        Log::warning('代理到期停用上游账号失败', [
                            'account_id' => $account->id,
                            'error' => $e->getMessage(),
                        ]);
                    }
                }
            }
        }

        return $stats;
    }

    // ===== 内部 =====

    /**
     * 管理员手工延长权益（补偿 / 客服处理，不产生订单）。
     *
     * 与续费的区别：不改密码、不重置额度，只把时间往后推；如果账号已被停用，
     * 顺手恢复一次可用凭据（否则用户有时间也连不上）。
     */
    public function extendEntitlement(ProxyEntitlement $entitlement, int $days): ProxyEntitlement
    {
        $days = max(1, $days);
        $base = $entitlement->expires_at && $entitlement->expires_at->isFuture()
            ? $entitlement->expires_at
            : now();

        $entitlement->forceFill([
            'expires_at' => $base->copy()->addDays($days),
            'status' => ProxyEntitlement::STATUS_ACTIVE,
        ])->save();

        $account = $entitlement->account_id ? ProxyAccount::find($entitlement->account_id) : null;
        if ($account) {
            $account->forceFill([
                'expires_at' => $entitlement->expires_at,
            ])->save();

            if ($account->status !== 'active') {
                try {
                    $this->proxyAccounts->renewAccount($account, [
                        'proxy_limit' => (float) ($entitlement->sku->traffic_gb ?? $account->proxy_limit),
                        'max_thread_count' => (int) ($entitlement->sku->max_thread_count ?? $account->max_thread_count),
                    ]);
                } catch (\Throwable $e) {
                    Log::warning('延长权益时恢复账号失败', [
                        'account_id' => $account->id,
                        'error' => $e->getMessage(),
                    ]);
                }
            }
        }

        return $entitlement;
    }

    /**
     * 把一条共享 socks 权益换到另一条线路（原线路挂了 / 要整体迁移）。
     */
    public function moveSocksEntitlement(ProxyEntitlement $entitlement, int $socksId): ProxyEntitlement
    {
        if ($entitlement->kind !== ProxyEntitlement::KIND_SOCKS) {
            throw new \InvalidArgumentException('只有共享 socks 权益能换线路');
        }

        $sock = ProxySocks::find($socksId);
        if (! $sock || ! $sock->is_active) {
            throw new \InvalidArgumentException('目标线路不存在或已停用');
        }

        $entitlement->forceFill(['socks_id' => $sock->id])->save();

        return $entitlement;
    }

    /** 同一用户、同货源类型、还有效（或可续费）的权益 */
    private function findRenewableEntitlement(User $user, ProxySku $sku, bool $includeExpired = false): ?ProxyEntitlement
    {
        $query = ProxyEntitlement::query()
            ->where('user_id', $user->id)
            ->where('kind', $sku->source_type === 'socks' ? ProxyEntitlement::KIND_SOCKS : ProxyEntitlement::KIND_WEBSHARE);

        if ($includeExpired) {
            $query->whereIn('status', [ProxyEntitlement::STATUS_ACTIVE, ProxyEntitlement::STATUS_EXPIRED, ProxyEntitlement::STATUS_DISABLED]);
        } else {
            $query->where('status', ProxyEntitlement::STATUS_ACTIVE)
                ->where(fn ($q) => $q->whereNull('expires_at')->orWhere('expires_at', '>', now()));
        }

        return $query->orderByDesc('id')->first();
    }

    /** 写/更新权益：续费时延长到期，不新增行 */
    private function upsertEntitlement(
        User $user,
        Order $order,
        ProxySku $sku,
        string $kind,
        ?int $accountId,
        ?int $socksId,
        ?ProxyEntitlement $existing,
    ): ProxyEntitlement {
        $days = max(1, (int) $sku->duration_days);

        if ($existing) {
            $base = $existing->expires_at && $existing->expires_at->isFuture() ? $existing->expires_at : now();
            $existing->forceFill([
                'order_id' => $order->id,
                'sku_id' => $sku->id,
                'status' => ProxyEntitlement::STATUS_ACTIVE,
                'expires_at' => $base->copy()->addDays($days),
                'account_id' => $accountId ?? $existing->account_id,
                'socks_id' => $socksId ?? $existing->socks_id,
            ])->save();

            return $existing;
        }

        return ProxyEntitlement::create([
            'user_id' => $user->id,
            'order_id' => $order->id,
            'sku_id' => $sku->id,
            'kind' => $kind,
            'account_id' => $accountId,
            'socks_id' => $socksId,
            'status' => ProxyEntitlement::STATUS_ACTIVE,
            'expires_at' => now()->addDays($days),
        ]);
    }

    /** 开一个 webshare 子账号并落库（额度 = 规格流量） */
    private function openWebshareAccount(Order $order, User $user, ProxySku $sku): ProxyAccount
    {
        $provider = $sku->source_id
            ? ProxyProvider::find($sku->source_id)
            : ProxyProvider::where('enabled', true)->orderBy('id')->first();

        if (! $provider) {
            throw new \RuntimeException('没有可用的 Webshare 账号（请先在上游里添加并启用）');
        }

        $account = $this->proxyAccounts->createAccount($provider, [
            'label' => 'u' . $user->id . '-' . $sku->name,
            'proxy_limit' => (float) $sku->traffic_gb,
            'max_thread_count' => (int) $sku->max_thread_count,
            'user_id' => $user->id,
        ]);

        $account->forceFill([
            'sku_id' => $sku->id,
            'traffic_limit_bytes' => $sku->traffic_gb > 0 ? $sku->traffic_gb * 1024 ** 3 : null,
            'order_id' => $order->id,
            'expires_at' => now()->addDays(max(1, (int) $sku->duration_days)),
        ])->save();

        return $account;
    }

    /** 续费：额度重置、恢复启用（换回可用密码）、延长到期 */
    private function renewWebshareAccount(ProxyAccount $account, ProxySku $sku): void
    {
        // 换密码 + 提回额度这一步交给 provider 服务（它负责与上游交互），
        // 这里只补本地的商城字段（sku / 到期 / 流量上限）。
        $this->proxyAccounts->renewAccount($account, [
            'proxy_limit' => (float) $sku->traffic_gb,
            'max_thread_count' => (int) $sku->max_thread_count,
        ]);

        $account->forceFill([
            'sku_id' => $sku->id,
            'traffic_limit_bytes' => $sku->traffic_gb > 0 ? $sku->traffic_gb * 1024 ** 3 : null,
            'expires_at' => ($account->expires_at && $account->expires_at->isFuture() ? $account->expires_at : now())
                ->copy()->addDays(max(1, (int) $sku->duration_days)),
        ])->save();
    }
}
