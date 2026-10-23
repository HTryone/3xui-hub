<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;

/**
 * 订单。
 */
class Order extends Model
{
    public const STATUS_PENDING = 'pending';
    public const STATUS_PAID = 'paid';
    public const STATUS_EXPIRED = 'expired';
    public const STATUS_CANCELLED = 'cancelled';

    public const TYPE_PLAN = 'plan';
    public const TYPE_PROXY = 'proxy';

    protected $fillable = [
        'order_no',
        'user_id',
        'order_type',
        'plan_id',
        'proxy_sku_id',
        'proxy_renewal',
        'amount',
        'original_amount',
        'discount_code_id',
        'discount_amount',
        'status',
        'payment_config_id',
        'trade_no',
        'paid_at',
        'pay_ip',
    ];

    protected function casts(): array
    {
        return [
            'amount' => 'decimal:2',
            'original_amount' => 'decimal:2',
            'discount_amount' => 'decimal:2',
            'discount_code_id' => 'integer',
            'proxy_sku_id' => 'integer',
            'proxy_renewal' => 'boolean',
            'paid_at' => 'datetime',
        ];
    }

    public function user(): BelongsTo
    {
        return $this->belongsTo(User::class);
    }

    public function plan(): BelongsTo
    {
        return $this->belongsTo(Plan::class);
    }

    /** 代理规格（仅 order_type=proxy） */
    public function proxySku(): BelongsTo
    {
        return $this->belongsTo(ProxySku::class, 'proxy_sku_id');
    }

    public function isProxyOrder(): bool
    {
        return $this->order_type === self::TYPE_PROXY;
    }

    public function paymentConfig(): BelongsTo
    {
        return $this->belongsTo(PaymentConfig::class);
    }

    /** 生成订单号 */
    public static function generateOrderNo(): string
    {
        return date('YmdHis') . str_pad(random_int(0, 999999), 6, '0', STR_PAD_LEFT);
    }
}
