<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;

/**
 * 机器流量采样（L0，5 分钟粒度）。
 *
 * sent / recv 是机器网卡的**累计**收发字节（3x-ui 的 netTraffic / xray agent 的
 * net_total_up|down），不是区间增量 —— 差分口径统一在上层（MonitorService），
 * 这样任意时点的口径都由同一条规则推出来，不存在「存增量时漏采一次就永久丢一段」的问题。
 *
 * 保留期由 config('monitor.retain_days') 决定；更旧的样本由 monitor:rollup 折算进
 * monitor_daily 后删除。
 */
#[Fillable(['node_id', 'sent', 'recv', 'cpu', 'mem', 'sampled_at'])]
class MonitorSample extends Model
{
    protected function casts(): array
    {
        return [
            'sent' => 'integer',
            'recv' => 'integer',
            'cpu' => 'float',
            'mem' => 'float',
            'sampled_at' => 'datetime',
        ];
    }

    public function node(): BelongsTo
    {
        return $this->belongsTo(Node::class);
    }
}
