<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\HasMany;

/**
 * 代理商品（对外展示层）：一个商品挂多个规格（SKU）。
 *
 * 与节点套餐（plans）完全独立：不参与节点/订阅注入，只用于代理商城展示。
 */
class ProxyProduct extends Model
{
    protected $table = 'proxy_products';

    protected $fillable = [
        'name',
        'subtitle',
        'description',
        'image_url',
        'is_active',
        'sort_order',
    ];

    protected function casts(): array
    {
        return [
            'is_active' => 'boolean',
            'sort_order' => 'integer',
        ];
    }

    public function skus(): HasMany
    {
        return $this->hasMany(ProxySku::class, 'product_id');
    }

    /** 上架规格（按 sort 排序） */
    public function activeSkus(): HasMany
    {
        return $this->skus()->where('is_active', true)->orderBy('sort_order')->orderBy('id');
    }
}
