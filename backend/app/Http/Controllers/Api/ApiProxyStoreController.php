<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Models\ProxyEntitlement;
use App\Models\ProxyProduct;
use App\Models\ProxySku;
use App\Models\SiteConfig;
use App\Services\ProxyStoreService;
use App\Services\Webshare\ProxyProviderService;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;

/**
 * 用户端代理商城。
 *
 * 和节点套餐完全独立：这里不出现流量/到期/重置这些订阅概念，
 * 只展示「代理商品 → 规格 → 价格 → 剩余份数」。
 */
class ApiProxyStoreController extends Controller
{
    use ApiResponse;

    public function __construct(
        private ProxyStoreService $store,
        private ProxyProviderService $proxyAccounts,
    ) {}

    /**
     * 代理商城总开关（SiteConfig: proxy_store_enabled，默认关闭）。
     *
     * 默认关闭的原因：代理是可选产品线，没开通上游/没配好价格前不应让用户看到；
     * 关闭时连接口一起拒（不只是隐藏菜单），否则直接调 /api/proxy-store/orders 就能下单。
     */
    private function ensureEnabled(): ?JsonResponse
    {
        if (SiteConfig::getValue('proxy_store_enabled', '0') !== '1') {
            return $this->error('代理商城暂未开放', 403);
        }

        return null;
    }

    /** 代理商城列表（只出上架商品与可售规格，不含成本价、不含货源细节） */
    public function products(): JsonResponse
    {
        if ($deny = $this->ensureEnabled()) {
            return $deny;
        }

        $products = ProxyProduct::query()
            ->where('is_active', true)
            ->with(['skus' => fn ($q) => $q->where('is_active', true)->orderBy('price')])
            ->orderBy('sort_order')
            ->orderBy('id')
            ->get();

        $items = $products->map(function (ProxyProduct $product) {
            $skus = $product->skus
                ->map(fn (ProxySku $sku) => [
                    'id' => $sku->id,
                    'name' => $sku->name,
                    'region' => $sku->region,
                    'traffic_gb' => (int) $sku->traffic_gb,
                    'duration_days' => (int) $sku->duration_days,
                    'max_thread_count' => (int) $sku->max_thread_count,
                    'price' => (float) $sku->price,
                    'stock_available' => $sku->availableStock(),
                    'kind' => $sku->source_type === 'socks' ? 'socks' : 'webshare',
                ])
                ->filter(fn (array $sku) => $sku['stock_available'] > 0)
                ->values();

            return [
                'id' => $product->id,
                'name' => $product->name,
                'subtitle' => $product->subtitle,
                'description' => $product->description,
                'skus' => $skus,
            ];
        })->filter(fn (array $p) => $p['skus']->isNotEmpty())->values();

        return $this->success($items);
    }

    /** 下单：返回支付链接（免费规格直接开通） */
    public function createOrder(Request $request): JsonResponse
    {
        if ($deny = $this->ensureEnabled()) {
            return $deny;
        }

        $data = $request->validate([
            'sku_id' => ['required', 'integer'],
            'payment_config_id' => ['sometimes', 'nullable', 'integer'],
        ]);

        try {
            $result = $this->store->createOrder(
                $request->user(),
                (int) $data['sku_id'],
                isset($data['payment_config_id']) ? (int) $data['payment_config_id'] : null,
            );
        } catch (\InvalidArgumentException $e) {
            return $this->error($e->getMessage(), 400);
        } catch (\Throwable $e) {
            report($e);

            return $this->error($e->getMessage() ?: '创建订单失败', 500);
        }

        return $this->success($result);
    }

    /** 我的代理：权益 + 凭据（更新到期的排前面） */
    public function entitlements(Request $request): JsonResponse
    {
        // 注意：这里**不校验**商城开关 —— 开关关闭只是停止售卖/隐藏入口，
        // 已经买过的人必须随时能看到自己的凭据（否则等于把人服务掐了）。

        $items = ProxyEntitlement::query()
            ->where('user_id', $request->user()->id)
            ->with(['sku', 'account.provider', 'socks'])
            ->orderByDesc('id')
            ->get()
            ->map(fn (ProxyEntitlement $e) => $this->presentEntitlement($e))
            ->values();

        return $this->success($items);
    }

    /** 某条权益的可用 IP 列表（webshare 实时拉；socks 直接回那条） */
    public function proxies(Request $request, int $id): JsonResponse
    {
        // 同上：已购权益的 IP 列表不受商城开关影响

        $entitlement = ProxyEntitlement::query()
            ->where('user_id', $request->user()->id)
            ->with(['account.provider', 'socks'])
            ->find($id);

        if (! $entitlement) {
            return $this->error('代理不存在', 404);
        }

        if ($entitlement->kind === ProxyEntitlement::KIND_SOCKS) {
            $sock = $entitlement->socks;
            if (! $sock) {
                return $this->error('货源已移除', 404);
            }

            return $this->success([[
                'address' => $sock->host,
                'port' => (int) $sock->port,
                'country' => (string) $sock->country,
                'city' => (string) $sock->city,
                'asn' => '',
                'valid' => true,
            ]]);
        }

        $account = $entitlement->account;
        if (! $account || ! $account->provider) {
            return $this->error('凭据不存在', 404);
        }

        try {
            $result = $this->proxyAccounts->listProxies($account->provider);
        } catch (\Throwable $e) {
            report($e);

            return $this->error('获取 IP 列表失败，请稍后重试', 502);
        }

        return $this->success($result['items']);
    }

    /**
     * 权益展示：密码只给 owner，且 status 不是 disabled 时才给 —— 停用后的密码是障眼法，
     * 给出去只会让用户白试。
     */
    private function presentEntitlement(ProxyEntitlement $entitlement): array
    {
        $sku = $entitlement->sku;
        $account = $entitlement->account;
        $sock = $entitlement->socks;

        $credentials = null;
        if ($entitlement->status !== ProxyEntitlement::STATUS_DISABLED) {
            if ($entitlement->kind === ProxyEntitlement::KIND_WEBSHARE && $account) {
                $credentials = [
                    'username' => (string) $account->username,
                    'password' => (string) $account->password,
                ];
            } elseif ($entitlement->kind === ProxyEntitlement::KIND_SOCKS && $sock) {
                $credentials = [
                    'username' => (string) ($sock->username ?? ''),
                    'password' => (string) ($sock->password ?? ''),
                ];
            }
        }

        return [
            'id' => $entitlement->id,
            'kind' => $entitlement->kind,
            'status' => $entitlement->isActive() ? 'active' : $entitlement->status,
            'sku_name' => $sku?->name,
            'traffic_gb' => (int) ($sku?->traffic_gb ?? 0),
            'duration_days' => (int) ($sku?->duration_days ?? 0),
            'expires_at' => $entitlement->expires_at?->toIso8601String(),
            'used_bytes' => $account ? (int) $account->used_bytes : null,
            'limit_bytes' => $account ? ($account->traffic_limit_bytes ? (int) $account->traffic_limit_bytes : null) : null,
            'credentials' => $credentials,
        ];
    }
}
