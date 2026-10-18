<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;

/**
 * 用户×节点 按小时流量（短期 L0）。
 *
 * 只服务「最近 24 小时」滑动窗口；长期统计看 UserTrafficDaily。
 * 三个 *_delta 列都是**区间增量**（累加型 upsert 写入），不是累计值。
 */
#[Fillable(['user_id', 'node_id', 'hour_start', 'up_delta', 'down_delta', 'weighted_delta'])]
class UserTrafficHourly extends Model
{
    protected $table = 'user_traffic_hourly';

    protected function casts(): array
    {
        return [
            'hour_start' => 'datetime',
            'up_delta' => 'integer',
            'down_delta' => 'integer',
            'weighted_delta' => 'integer',
        ];
    }

    public function user(): BelongsTo
    {
        return $this->belongsTo(User::class);
    }

    public function node(): BelongsTo
    {
        return $this->belongsTo(Node::class);
    }
}
