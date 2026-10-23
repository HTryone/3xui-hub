<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;

/**
 * 用户代理权益：用户买了哪个代理规格、到什么时候、凭据落在哪。
 *
 * kind=webshare → account_id 指向 proxy_accounts（独占子账号）
 * kind=socks    → socks_id 指向 proxy_socks（多人共用的同一套凭据）
 */
class ProxyEntitlement extends Model
{
    public const KIND_WEBSHARE = 'webshare';
    public const KIND_SOCKS = 'socks';

    public const STATUS_ACTIVE = 'active';
    public const STATUS_EXPIRED = 'expired';
    public const STATUS_DISABLED = 'disabled';

    protected $table = 'proxy_entitlements';

    protected $fillable = [
        'user_id',
        'order_id',
        'sku_id',
        'kind',
        'account_id',
        'socks_id',
        'expires_at',
        'status',
    ];

    protected function casts(): array
    {
        return [
            'user_id' => 'integer',
            'order_id' => 'integer',
            'sku_id' => 'integer',
            'account_id' => 'integer',
            'socks_id' => 'integer',
            'expires_at' => 'datetime',
        ];
    }

    public function user(): BelongsTo
    {
        return $this->belongsTo(User::class);
    }

    public function sku(): BelongsTo
    {
        return $this->belongsTo(ProxySku::class, 'sku_id');
    }

    public function account(): BelongsTo
    {
        return $this->belongsTo(ProxyAccount::class, 'account_id');
    }

    public function socks(): BelongsTo
    {
        return $this->belongsTo(ProxySocks::class, 'socks_id');
    }

    public function isActive(): bool
    {
        return $this->status === self::STATUS_ACTIVE
            && ($this->expires_at === null || $this->expires_at->isFuture());
    }
}
