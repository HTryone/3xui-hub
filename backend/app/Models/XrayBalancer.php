<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;

/**
 * xray 节点路由负载均衡器（按 sort 渲染进 routing.balancers）。
 *
 * selector 为出站 tag 前缀数组（xray 前缀匹配），strategy 决定选路算法：
 * random / roundRobin / leastPing / leastLoad。leastPing / leastLoad 需要
 * 观测器数据，渲染层据启用的 balancer 自动补 observatory / burstObservatory。
 */
#[Fillable([
    'node_id',
    'tag',
    'selectors',
    'strategy',
    'fallback_tag',
    'remark',
    'enabled',
    'sort',
])]
class XrayBalancer extends Model
{
    public const STRATEGIES = ['random', 'roundRobin', 'leastPing', 'leastLoad'];

    /** 需要观测器（leastPing 用 observatory，leastLoad 用 burstObservatory）。 */
    public const OBSERVED_STRATEGIES = ['leastPing', 'leastLoad'];

    protected function casts(): array
    {
        return [
            'selectors' => 'array',
            'enabled' => 'boolean',
            'sort' => 'integer',
        ];
    }

    public function node(): BelongsTo
    {
        return $this->belongsTo(Node::class);
    }

    public static function enabledFor(Node $node): \Illuminate\Database\Eloquent\Collection
    {
        return static::where('node_id', $node->id)
            ->where('enabled', true)
            ->orderBy('sort')
            ->orderBy('id')
            ->get();
    }
}
