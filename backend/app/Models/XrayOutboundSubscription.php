<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;

/**
 * 出站订阅（服务器池，per-node）。
 *
 * 一条订阅 = 一个外部订阅 URL，后台定时拉取分享链接 → 解析成出站 → 按稳定 tag
 * 注入该节点的 config（XrayConfigService::renderOutbounds 合并）。
 *
 * - fetched：最近一次成功拉取的出站数组（渲染直接读它，为空则不注入）
 * - link_identities：identity → tag 映射，下次刷新据此复用 tag（保稳定）
 * - content_hash：内容指纹，变化才 bump 节点 config_version（免无谓重载）
 */
class XrayOutboundSubscription extends Model
{
    protected $table = 'xray_outbound_subscriptions';

    protected $fillable = [
        'node_id',
        'remark',
        'url',
        'tag_prefix',
        'update_interval',
        'prepend',
        'priority',
        'enabled',
        'fetched',
        'link_identities',
        'content_hash',
        'node_count',
        'last_fetched_at',
        'last_status',
        'last_error',
    ];

    protected function casts(): array
    {
        return [
            'enabled' => 'boolean',
            'prepend' => 'boolean',
            'update_interval' => 'integer',
            'priority' => 'integer',
            'node_count' => 'integer',
            'last_fetched_at' => 'datetime',
        ];
    }

    public function node(): BelongsTo
    {
        return $this->belongsTo(Node::class);
    }

    /** 最近一次成功拉取解析出的出站（数组）；无/损坏返回空数组。 */
    public function outbounds(): array
    {
        if (! is_string($this->fetched) || trim($this->fetched) === '') {
            return [];
        }
        $decoded = json_decode($this->fetched, true);

        return is_array($decoded) ? array_values(array_filter($decoded, 'is_array')) : [];
    }

    /** identity → tag 映射（损坏/无则空数组）。 */
    public function identities(): array
    {
        if (! is_string($this->link_identities) || trim($this->link_identities) === '') {
            return [];
        }
        $decoded = json_decode($this->link_identities, true);

        return is_array($decoded) ? $decoded : [];
    }

    /** 启用的订阅（节点维度，按 priority 排序）。 */
    public static function enabledFor(Node $node): \Illuminate\Database\Eloquent\Collection
    {
        return static::where('node_id', $node->id)
            ->where('enabled', true)
            ->orderBy('priority')
            ->orderBy('id')
            ->get();
    }

    /**
     * 全部启用订阅的「最近一次拉取出的出站」——渲染时合并进 config。
     *
     * 排序：priority 升序（同组内顺序可控），id 兜底。prepend=true 的排在
     * 自定义出站之前，false 的排在其后（对齐 3x-ui activeOutboundsSplit）。
     *
     * @return array{prepend: array<int,array>, append: array<int,array>}
     */
    public static function activeOutboundsSplit(Node $node): array
    {
        $prepend = [];
        $append = [];
        foreach (static::enabledFor($node) as $sub) {
            foreach ($sub->outbounds() as $ob) {
                $tag = (string) ($ob['tag'] ?? '');
                // 无 tag 或占用保留 tag 的一律丢弃（防污染内置 direct/blocked/api）
                if ($tag === '' || in_array($tag, ['direct', 'blocked', 'api'], true)) {
                    continue;
                }
                if ($sub->prepend) {
                    $prepend[] = $ob;
                } else {
                    $append[] = $ob;
                }
            }
        }

        return ['prepend' => $prepend, 'append' => $append];
    }

    /** 全部启用订阅的 tag 集合（路由候选 / 校验用）。 */
    public static function activeTags(Node $node): array
    {
        $tags = [];
        foreach (static::enabledFor($node) as $sub) {
            foreach ($sub->outbounds() as $ob) {
                $tag = (string) ($ob['tag'] ?? '');
                if ($tag !== '') {
                    $tags[$tag] = true;
                }
            }
        }

        return array_keys($tags);
    }
}
