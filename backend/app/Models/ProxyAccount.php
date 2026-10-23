<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;

/**
 * 上游子用户（= 平台用户拿到的独立代理凭据）。
 *
 * password 用 encrypted cast 加密存储，并在 $hidden 里剔除：列表/详情接口
 * 不会带出明文，也不会带出密文。停用后密码已被改成随机串，本字段不再可用。
 */
class ProxyAccount extends Model
{
    protected $table = 'proxy_accounts';

    protected $fillable = [
        'provider_id',
        'upstream_id',
        'user_id',
        'label',
        'username',
        'password',
        'proxy_limit',
        'traffic_limit_bytes',
        'expires_at',
        'order_id',
        'sku_id',
        'max_thread_count',
        'proxy_countries',
        'used_bytes',
        'status',
        'last_synced_at',
    ];

    protected $hidden = ['password'];

    protected function casts(): array
    {
        return [
            'password' => 'encrypted',
            'proxy_countries' => 'array',
            'proxy_limit' => 'float',
            'traffic_limit_bytes' => 'integer',
            'expires_at' => 'datetime',
            'order_id' => 'integer',
            'sku_id' => 'integer',
            'max_thread_count' => 'integer',
            'used_bytes' => 'integer',
            'last_synced_at' => 'datetime',
        ];
    }

    public function provider(): BelongsTo
    {
        return $this->belongsTo(ProxyProvider::class, 'provider_id');
    }

    public function user(): BelongsTo
    {
        return $this->belongsTo(User::class);
    }

    /** 购买的规格（手动开的号为空） */
    public function sku(): BelongsTo
    {
        return $this->belongsTo(ProxySku::class, 'sku_id');
    }

    /** 首次开通的订单 */
    /** 该账号最新的代理权益（运营页列表用：一次预加载，别按行查） */
    public function latestEntitlement(): \Illuminate\Database\Eloquent\Relations\HasOne
    {
        return $this->hasOne(ProxyEntitlement::class, 'account_id')->latestOfMany();
    }
    public function order(): BelongsTo
    {
        return $this->belongsTo(Order::class, 'order_id');
    }

    /** 商城购买来的账号才有到期时间；手动开的号不受到期任务影响 */
    public function isStoreManaged(): bool
    {
        return $this->sku_id !== null;
    }
}
