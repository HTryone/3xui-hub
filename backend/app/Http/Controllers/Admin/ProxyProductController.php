<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\ProxyProduct;
use App\Models\ProxySku;
use App\Models\ProxySocks;
use App\Models\SiteConfig;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;

/**
 * 代理商城 · 商品与规格管理（/admin-api/proxy-store）。
 *
 * 与节点套餐（plans）完全分开：这张表只用于「代理商城」，不影响节点/订阅。
 * 库存三态在 ProxySku 里维护（total / locked / sold），后台可直接改 total
 * （站长在 Webshare 加购子用户后，把可售数量调大）。
 */
class ProxyProductController extends Controller
{
    use ApiResponse;

    /**
     * 代理商城总开关。
     *
     * GET  /admin-api/proxy-store/settings → { enabled }
     * POST /admin-api/proxy-store/settings → 改开关
     *
     * 默认关闭：代理是可选产品线，商品/价格/上游没配好之前，用户端不该看到入口，
     * 也不该能调用户接口下单（ApiProxyStoreController 里同步校验这个开关）。
     * 商品与规格管理（本控制器其它接口）不受影响，后台随时可编辑。
     */
    public function settings(): JsonResponse
    {
        return $this->success(['enabled' => SiteConfig::getValue('proxy_store_enabled', '0') === '1']);
    }

    /** POST /admin-api/proxy-store/settings */
    public function updateSettings(Request $request): JsonResponse
    {
        $data = $request->validate(['enabled' => ['required', 'boolean']]);

        SiteConfig::setValue('proxy_store_enabled', $data['enabled'] ? '1' : '0');

        return $this->success(
            ['enabled' => (bool) $data['enabled']],
            $data['enabled'] ? '代理商城已开启，用户端可见' : '代理商城已关闭，用户端不再显示'
        );
    }

    /** GET /admin-api/proxy-store/products —— 商品列表（带规格） */
    public function index(): JsonResponse
    {
        $products = ProxyProduct::query()
            ->with(['skus' => fn ($q) => $q->orderBy('sort_order')->orderBy('id')])
            ->orderBy('sort_order')
            ->orderByDesc('id')
            ->get();

        return $this->success([
            'items' => $products->map(fn (ProxyProduct $p) => $this->presentProduct($p))->values(),
        ]);
    }

    /** POST /admin-api/proxy-store/products */
    public function store(Request $request): JsonResponse
    {
        $data = $this->validateProduct($request);
        $product = ProxyProduct::create($data);

        return $this->success($this->presentProduct($product->load('skus')), '已创建');
    }

    /** PUT /admin-api/proxy-store/products/{product} */
    public function update(Request $request, ProxyProduct $product): JsonResponse
    {
        $data = $this->validateProduct($request);
        $product->fill($data)->save();

        return $this->success($this->presentProduct($product->fresh()->load('skus')), '已保存');
    }

    /** DELETE /admin-api/proxy-store/products/{product} */
    public function destroy(ProxyProduct $product): JsonResponse
    {
        // 已售出的规格不允许连带删除（订单/账号还引用它的额度与时长）
        $sold = $product->skus()->where('stock_sold', '>', 0)->exists();
        if ($sold) {
            return $this->error('该商品已有售出的规格，不能删除；如需下架请关掉「上架」开关', 400);
        }

        $product->skus()->delete();
        $product->delete();

        return $this->success(null, '已删除');
    }

    /** POST /admin-api/proxy-store/products/{product}/skus */
    public function storeSku(Request $request, ProxyProduct $product): JsonResponse
    {
        $data = $this->validateSku($request);
        $sku = $product->skus()->create($data);

        return $this->success($this->presentSku($sku), '已添加规格');
    }

    /** PUT /admin-api/proxy-store/skus/{sku} */
    public function updateSku(Request $request, ProxySku $sku): JsonResponse
    {
        $data = $this->validateSku($request);
        $sku->fill($data)->save();

        return $this->success($this->presentSku($sku->fresh()), '已保存');
    }

    /** DELETE /admin-api/proxy-store/skus/{sku} */
    public function destroySku(ProxySku $sku): JsonResponse
    {
        if ((int) $sku->stock_sold > 0) {
            return $this->error('该规格已有售出记录，不能删除；请关闭「启用」来下架', 400);
        }
        if ((int) $sku->stock_locked > 0) {
            return $this->error('该规格有未支付订单占用着库存，请等订单超时后再删', 400);
        }

        $sku->delete();

        return $this->success(null, '已删除');
    }

    // ===== 内部 =====

    private function validateProduct(Request $request): array
    {
        return $request->validate([
            'name' => ['required', 'string', 'max:100'],
            'subtitle' => ['nullable', 'string', 'max:200'],
            'description' => ['nullable', 'string', 'max:5000'],
            'image_url' => ['nullable', 'string', 'max:500'],
            'is_active' => ['sometimes', 'boolean'],
            'sort_order' => ['sometimes', 'integer', 'min:0'],
        ]);
    }

    private function validateSku(Request $request): array
    {
        $data = $request->validate([
            'name' => ['required', 'string', 'max:100'],
            // 货源：webshare=开上游子账号（独占）；socks=共用一条手工 socks（共享）
            'source_type' => ['sometimes', 'in:webshare,socks'],
            'allocation' => ['sometimes', 'in:exclusive,shared'],
            'source_id' => ['nullable', 'integer', 'min:1'],
            'region' => ['nullable', 'string', 'max:32'],
            // 0 = 不限（socks 共享型交给上游自己管）
            'traffic_gb' => ['sometimes', 'integer', 'min:0'],
            'duration_days' => ['required', 'integer', 'min:1'],
            'max_thread_count' => ['sometimes', 'integer', 'min:0'],
            'price' => ['required', 'numeric', 'min:0'],
            'cost_price' => ['sometimes', 'numeric', 'min:0'],
            'stock_total' => ['sometimes', 'integer', 'min:0'],
            'is_active' => ['sometimes', 'boolean'],
            'sort_order' => ['sometimes', 'integer', 'min:0'],
        ]);

        // socks 货源只能共享，且必须指定具体那条 socks
        if (($data['source_type'] ?? 'webshare') === 'socks') {
            if (empty($data['source_id'])) {
                throw \Illuminate\Validation\ValidationException::withMessages([
                    'source_id' => 'socks 货源必须选择一条已录入的 socks',
                ]);
            }
            if (! ProxySocks::whereKey($data['source_id'])->exists()) {
                throw \Illuminate\Validation\ValidationException::withMessages([
                    'source_id' => '选择的 socks 不存在',
                ]);
            }
            $data['allocation'] = 'shared';
        }

        return $data;
    }

    private function presentProduct(ProxyProduct $product): array
    {
        return [
            'id' => $product->id,
            'name' => $product->name,
            'subtitle' => $product->subtitle,
            'description' => $product->description,
            'image_url' => $product->image_url,
            'is_active' => (bool) $product->is_active,
            'sort_order' => (int) $product->sort_order,
            'skus' => $product->skus->map(fn (ProxySku $s) => $this->presentSku($s))->values(),
            'created_at' => $product->created_at?->toDateTimeString(),
        ];
    }

    private function presentSku(ProxySku $sku): array
    {
        return [
            'id' => $sku->id,
            'product_id' => $sku->product_id,
            'name' => $sku->name,
            // 货源类型 + 分配方式：前端据此显示「独占账号 / 多人共用」
            'source_type' => $sku->source_type,
            'allocation' => $sku->allocation,
            'source_id' => $sku->source_id,
            'region' => $sku->region,
            'traffic_gb' => (int) $sku->traffic_gb,
            'duration_days' => (int) $sku->duration_days,
            'max_thread_count' => (int) $sku->max_thread_count,
            'price' => (float) $sku->price,
            'cost_price' => (float) $sku->cost_price,
            // 三个库存数字都给，前台/后台各自按需展示
            'stock_total' => (int) $sku->stock_total,
            'stock_locked' => (int) $sku->stock_locked,
            'stock_sold' => (int) $sku->stock_sold,
            'stock_available' => $sku->availableStock(),
            'is_active' => (bool) $sku->is_active,
            'sort_order' => (int) $sku->sort_order,
        ];
    }
}
