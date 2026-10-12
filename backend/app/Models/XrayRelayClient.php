<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;

/**
 * xray 中转凭据：源节点的中转出站连到目标节点入站时，面板在目标入站上自动
 * 生成的一条 client 凭据（渲染进该入站 clients，照 XrayConfigService::clientsFor）。
 *
 * 一行 = 某个「源节点 × 目标入站」组合的一对凭据；source_node_id / source_inbound_id
 * 记录来源，供删除中转出站时回收（见 XrayRelayService）。
 */
#[Fillable([
    'node_id',
    'inbound_id',
    'email',
    'client_id',
    'source_node_id',
    'source_inbound_id',
])]
class XrayRelayClient extends Model
{
    /** 目标节点入站（凭据所属）。 */
    public function node(): BelongsTo
    {
        return $this->belongsTo(Node::class);
    }

    /** 目标入站。 */
    public function inbound(): BelongsTo
    {
        return $this->belongsTo(XrayInbound::class, 'inbound_id');
    }

    /** 渲染用：某 xray 入站的全部 relay 凭据。 */
    public static function forInbound(XrayInbound $inbound): \Illuminate\Database\Eloquent\Collection
    {
        return static::where('inbound_id', $inbound->id)
            ->orderBy('id')
            ->get();
    }

    /**
     * 渲染成 xray 入站 clients 片段（照 clientsFor 口径，不分协议差异：
     * vless/vmess 用 id 字段，trojan 用 password 字段）。
     *
     * @return array<int, array>
     */
    public static function clientsForInbound(XrayInbound $inbound): array
    {
        $clients = [];
        foreach (static::forInbound($inbound) as $rc) {
            if ($inbound->protocol === 'trojan') {
                $clients[] = ['password' => (string) $rc->client_id];
            } else {
                $clients[] = ['id' => (string) $rc->client_id];
            }
        }

        return $clients;
    }
}
