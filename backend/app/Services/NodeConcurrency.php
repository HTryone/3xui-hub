<?php

namespace App\Services;

use App\Models\Node;

/**
 * 节点级批量操作的并发口径（重置流量 / 启停 / 流量同步 / 建号共用）。
 *
 * 为什么只放一部分节点进并发组（与订阅、健康检查同一套判定）：
 * - 3x-ui + api_key（Bearer）：无状态，只加 Authorization 头，可安全并发；
 * - 3x-ui + cookie 模式：要维护登录会话（/login + CSRF）并共享 cookie jar，
 *   并发会互相踩登录态 → 必须回退同步；
 * - xray 节点：本地拼指令 / 读库，没有跨机等待，串行即可。
 *
 * 判定本身若抛异常（节点属性读取失败，如 api_key 密文损坏），一律按不可并发处理，
 * 交给各处的同步兜底去 try/catch，保持拆分前的结论。
 */
final class NodeConcurrency
{
    /** 该节点能否走并发组。 */
    public static function eligible(Node $node): bool
    {
        try {
            return ($node->driver_type ?? '3x-ui') === '3x-ui'
                && (string) ($node->api_key ?? '') !== '';
        } catch (\Throwable) {
            return false;
        }
    }

    /** 同进程并发上限：panel.node_ops_concurrency（默认 10）；<= 0 视为 1（逐批单个，不并发）。 */
    public static function limit(): int
    {
        $limit = (int) config('panel.node_ops_concurrency', 10);

        return $limit > 0 ? $limit : 1;
    }
}
