<?php

namespace App\Services;

use App\Models\Node;
use App\Models\NodeApiCommand;
use Illuminate\Support\Facades\Cache;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Log;

/**
 * xray 节点指令队列共享操作（HTTP 长轮询通道与 WS 推送通道共用）。
 *
 * 两条通道必须共享「原子认领 / 回执落库」的同一实现：
 * 否则双通道并发时会出现同一指令被取走两次、或回执状态互相覆盖。
 */
class NodeApiCommandService
{
    /**
     * 回执输出落库上限（按 action 放宽）。
     *
     * reality_scan 的回执是一整份结果 JSON（多目标 × 证书/ALPN/延迟等字段），
     * 按默认 2000 字截断会把 JSON 砍成非法串直接丢结果，故单独放宽。
     */
    private const OUTPUT_LIMITS = [
        NodeApiCommand::ACTION_REALITY_SCAN => 30000,
    ];

    private const OUTPUT_LIMIT_DEFAULT = 2000;

    /**
     * 原子认领某节点的 pending 指令（pending → acknowledged）。
     *
     * @return array<int, array{id:int, action:string, payload:mixed}>
     */
    public function claimPending(Node $node, int $limit = 5): array
    {
        $out = [];
        foreach (NodeApiCommand::pendingFor($node->id, $limit) as $command) {
            if (! $command->acknowledge()) {
                continue; // 并发下被另一条通道抢走
            }
            $out[] = [
                'id' => $command->id,
                'action' => $command->action,
                'payload' => $command->payload,
            ];
        }

        return $out;
    }

    /**
     * 应用 agent 的指令回执（success/failed + 输出摘录）。
     *
     * @param  array<int, array{id?:int, success?:bool, output?:string}>  $results
     * @return int 更新条数
     */
    public function applyAck(Node $node, array $results): int
    {
        $updated = 0;
        foreach ($results as $row) {
            if (! is_array($row) || ! isset($row['id'])) {
                continue;
            }

            $command = NodeApiCommand::where('id', (int) $row['id'])
                ->where('node_id', $node->id)
                ->first();
            if ($command === null) {
                continue;
            }

            $ok = (bool) ($row['success'] ?? false);
            $limit = self::OUTPUT_LIMITS[$command->action] ?? self::OUTPUT_LIMIT_DEFAULT;
            $command->complete(
                $ok,
                isset($row['output']) ? mb_substr((string) $row['output'], 0, $limit) : null
            );
            $updated++;

            if (! $ok) {
                Log::warning('xray node command failed', [
                    'node_id' => $node->id,
                    'command_id' => $command->id,
                    'action' => $command->action,
                    'output' => mb_substr((string) ($row['output'] ?? ''), 0, 500),
                ]);
            }
        }

        // 顺带清理过期终态回执（懒删除，每小时至多一次，失败只记 warning）
        $this->pruneExpired();

        return $updated;
    }

    /**
     * 终态回执过期清理（懒删除：跟着回执落库顺带做，每小时至多一次）。
     *
     * 为什么不做定时任务：这张表只在有指令时增长，清理跟着写入走天然同生命周期
     * —— 没有指令就没有新写入，也就不需要清。DELETE 走 status 索引 + LIMIT，
     * 毫秒级；独立 try/catch 只记 warning，绝不影响指令投递与回执落库。
     *
     * 只删 success / failed 且过期的行：pending / acknowledged 还可能被 agent
     * 取走或在途，一律不动。config('xray.command_retain_days') 设 0 即关闭。
     */
    private function pruneExpired(): void
    {
        $retainDays = (int) config('xray.command_retain_days', 30);
        if ($retainDays <= 0) {
            return; // 0 = 关闭清理
        }

        if (! Cache::add('node-api-commands:prune', 1, 3600)) {
            return; // 这一小时内已经清过
        }

        try {
            DB::table('node_api_commands')
                ->whereIn('status', [NodeApiCommand::STATUS_SUCCESS, NodeApiCommand::STATUS_FAILED])
                ->where('updated_at', '<', now()->subDays($retainDays))
                ->limit(5000)
                ->delete();
        } catch (\Throwable $e) {
            Log::warning('node_api_commands 过期清理失败（不影响指令投递）', ['error' => $e->getMessage()]);
        }
    }
}
