<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;

/**
 * 代理规格（SKU）：真正售卖与开通的单元。
 *
 * 库存三态：
 *   stock_total   可售总数（= 上游还能开几个子用户，站长手动维护）
 *   stock_locked  已下单待支付占用
 *   stock_sold    已支付成功（已开号）
 *
 * 下单 reserve → 支付成功 consume → 订单超时/取消 release，
 * 三个动作都用**条件更新**（total - locked - sold >= n 才成功），并发不会超卖。
 */
class ProxySku extends Model
{
    protected $table = 'proxy_skus';

    protected $fillable = [
        'product_id',
        'name',
        'source_type',
        'allocation',
        'source_id',
        'region',
        'traffic_gb',
        'duration_days',
        'max_thread_count',
        'price',
        'cost_price',
        'stock_total',
        'stock_locked',
        'stock_sold',
        'is_active',
        'sort_order',
    ];

    protected function casts(): array
    {
        return [
            'traffic_gb' => 'integer',
            'duration_days' => 'integer',
            'max_thread_count' => 'integer',
            'source_id' => 'integer',
            'price' => 'decimal:2',
            'cost_price' => 'decimal:2',
            'stock_total' => 'integer',
            'stock_locked' => 'integer',
            'stock_sold' => 'integer',
            'is_active' => 'boolean',
            'sort_order' => 'integer',
        ];
    }

    public function product(): BelongsTo
    {
        return $this->belongsTo(ProxyProduct::class, 'product_id');
    }

    /** 共享型 socks 货源（仅 source_type=socks） */
    public function socks(): BelongsTo
    {
        return $this->belongsTo(ProxySocks::class, 'source_id');
    }

    /** 独占型子账号（仅 source_type=webshare） */
    public function provider(): BelongsTo
    {
        return $this->belongsTo(ProxyProvider::class, 'source_id');
    }

    /** 共享型：多人共用同一套凭据；独占型：一人一个上游子账号 */
    public function isShared(): bool
    {
        return $this->allocation === 'shared';
    }

    /** 当前可售数量 */
    public function availableStock(): int
    {
        return max(0, (int) $this->stock_total - (int) $this->stock_locked - (int) $this->stock_sold);
    }

    /** 下单占用（支付前）。返回 false = 库存不足。 */
    public static function reserveStock(int $skuId, int $qty = 1): bool
    {
        if ($qty < 1) {
            return false;
        }

        $affected = static::whereKey($skuId)
            ->whereRaw('stock_total - stock_locked - stock_sold >= ?', [$qty])
            ->update(['stock_locked' => \DB::raw('stock_locked + ' . $qty)]);

        return $affected > 0;
    }

    /** 释放占用（订单取消/超时/开号失败）。 */
    public static function releaseStock(int $skuId, int $qty = 1): void
    {
        if ($qty < 1) {
            return;
        }

        static::whereKey($skuId)
            ->whereRaw('stock_locked >= ?', [$qty])
            ->update(['stock_locked' => \DB::raw('stock_locked - ' . $qty)]);
    }

    /** 支付成功：占用转已售。 */
    public static function consumeStock(int $skuId, int $qty = 1): void
    {
        if ($qty < 1) {
            return;
        }

        static::whereKey($skuId)
            ->whereRaw('stock_locked >= ?', [$qty])
            ->update([
                'stock_locked' => \DB::raw('stock_locked - ' . $qty),
                'stock_sold' => \DB::raw('stock_sold + ' . $qty),
            ]);
    }
}
