<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;

/**
 * 用户×节点 按天流量（永久保留）。
 *
 * 与 UserTrafficHourly 每轮同步同时写入，不依赖小时表汇总：
 * 天表从上线第一天起就是完整的，小时表过期删除不会影响 7d/30d。
 */
#[Fillable(['user_id', 'node_id', 'date', 'up_delta', 'down_delta', 'weighted_delta'])]
class UserTrafficDaily extends Model
{
    protected $table = 'user_traffic_daily';

    protected function casts(): array
    {
        return [
            'date' => 'date',
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
