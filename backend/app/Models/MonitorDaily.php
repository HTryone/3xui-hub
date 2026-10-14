<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;

/**
 * 机器流量天聚合（永久保留）。
 *
 * sent_delta / recv_delta 是「该自然日（站点时区）的增量字节」，由 monitor:rollup
 * 从过期的 L0 样本差分折出来。原始样本删除后结论仍在这里，采样表不随时间无界增长。
 *
 * 口径与 series 接口一致：组内按 sampled_at 升序，每条减前一条（可落在组外，
 * 取该天之前最近一条），负差钳 0 后求和。
 */
#[Fillable(['node_id', 'date', 'sent_delta', 'recv_delta'])]
class MonitorDaily extends Model
{
    protected $table = 'monitor_daily';

    protected function casts(): array
    {
        return [
            'date' => 'date',
            'sent_delta' => 'integer',
            'recv_delta' => 'integer',
        ];
    }

    public function node(): BelongsTo
    {
        return $this->belongsTo(Node::class);
    }
}
