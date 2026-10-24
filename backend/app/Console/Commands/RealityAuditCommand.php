<?php

namespace App\Console\Commands;

use App\Drivers\Xray\XrayConfigService;
use App\Models\Node;
use App\Models\NodeApiCommand;
use App\Models\OperationLog;
use App\Models\XrayInbound;
use Illuminate\Console\Command;

/**
 * REALITY 入站巡检（issue #57）。
 *
 * 校验的两件事都在**节点侧**完成，面板不做任何 TLS 探测：
 *   1. dest 是否还能正常握手（feasible）
 *   2. dest 返回的证书链是否超过 REALITY 的 8192 字节上限（Xray-core #6356）
 *   3. 入站配置的每个 serverName 是否都在 dest 证书覆盖范围（SAN）内
 *
 * 为什么不在面板侧探测：面板与节点的网络视角完全不同（
 * 见 agent/reality_scan.go 注释），只有节点视角的握手才等于用户实际拿到的结果。
 *
 * 只告警、不自动改配置：自动摘 serverNames 会静默改掉管理员配的东西，
 * 而且证书链大小会随时间变化（微软 8251 就是后来涨上去的），告警比自动动手更可控。
 */
class RealityAuditCommand extends Command
{
    protected $signature = 'xray:reality-audit
                            {--node= : 只巡检指定节点 id}
                            {--wait=20 : 每个节点等待回执的秒数}';

    protected $description = 'REALITY 巡检：节点侧探测 dest，告警 SNI 不匹配与证书链超限（issue #57）';

    public function handle(): int
    {
        $nodes = Node::where('enabled', true)->get()->filter(fn (Node $n) => $n->isXray());
        if ($id = $this->option('node')) {
            $nodes = $nodes->where('id', (int) $id);
        }

        $inbounds = XrayInbound::whereIn('node_id', $nodes->pluck('id'))
            ->where('enabled', true)
            ->orderBy('node_id')
            ->orderBy('sort')
            ->orderBy('id')
            ->get()
            ->filter(fn (XrayInbound $in) => ($in->stream_settings['security'] ?? '') === 'reality');

        if ($inbounds->isEmpty()) {
            $this->line('没有启用中的 REALITY 入站');

            return self::SUCCESS;
        }

        $alerts = 0;

        foreach ($inbounds->groupBy('node_id') as $nodeId => $group) {
            $node = $nodes->firstWhere('id', (int) $nodeId);
            if ($node === null) {
                continue;
            }

            $targets = [];
            foreach ($group as $inbound) {
                $dest = trim((string) ($inbound->stream_settings['realitySettings']['dest'] ?? ''));
                if ($dest !== '') {
                    $targets[$dest] = true;
                }
            }
            if ($targets === []) {
                continue;
            }

            $results = $this->probe($node, array_keys($targets));
            if ($results === null) {
                $this->warn("{$node->name}：节点未回执（离线 / agent 过旧），本轮跳过");

                continue;
            }

            foreach ($group as $inbound) {
                foreach ($this->issuesFor($inbound, $results) as $issue) {
                    $alerts++;
                    $message = sprintf(
                        '%s（节点 %s，入站 %s，端口 %d）',
                        $issue,
                        $node->name,
                        $inbound->tag !== '' ? $inbound->tag : '#' . $inbound->id,
                        (int) $inbound->port
                    );
                    $this->warn($message);
                    $this->record($message);
                }
            }
        }

        $this->info($alerts === 0
            ? 'REALITY 巡检通过，未发现问题'
            : "REALITY 巡检完成，{$alerts} 项告警已记入操作日志");

        return self::SUCCESS;
    }

    /**
     * 单条入站的问题清单（空数组 = 通过）。
     *
     * @param  array<string,array>  $results  dest host => 节点回执条目
     * @return array<int,string>
     */
    private function issuesFor(XrayInbound $inbound, array $results): array
    {
        $dest = trim((string) ($inbound->stream_settings['realitySettings']['dest'] ?? ''));
        $host = XrayConfigService::hostOf($dest);
        $row = $results[$host] ?? null;

        if ($row === null) {
            return ["dest 探测未返回结果：{$dest}"];
        }

        $issues = [];

        if (empty($row['feasible'])) {
            $reason = trim((string) ($row['reason'] ?? ''));
            $issues[] = 'dest 无法完成 REALITY 所需的 TLS 握手' . ($reason !== '' ? "：{$reason}" : '');
        }

        if (! empty($row['chain_warn'])) {
            $issues[] = sprintf(
                'dest 证书链 %d 字节超过 REALITY 的 8192 上限，握手会被截断（Xray-core #6356）',
                (int) ($row['chain_bytes'] ?? 0)
            );
        }

        $san = [];
        foreach ((array) ($row['server_names'] ?? []) as $name) {
            if (is_string($name) && trim($name) !== '') {
                $san[] = strtolower(trim($name));
            }
        }

        // 节点没回 SAN 时无法判断，不瞎报
        if ($san !== []) {
            foreach ((array) ($inbound->stream_settings['realitySettings']['serverNames'] ?? []) as $sni) {
                if (! is_string($sni) || trim($sni) === '') {
                    continue;
                }
                $sni = strtolower(trim($sni));
                if (! in_array($sni, $san, true)) {
                    $issues[] = "serverName {$sni} 不在 dest 证书覆盖范围内（被分到它的用户会握手失败）";
                }
            }
        }

        return $issues;
    }

    /**
     * 下发一次 reality_scan 并等回执。
     *
     * @param  array<int,string>  $targets
     * @return array<string,array>|null  dest host => 回执条目；节点没回执返回 null
     */
    private function probe(Node $node, array $targets): ?array
    {
        $command = NodeApiCommand::create([
            'node_id' => $node->id,
            'user_id' => null,
            'action' => NodeApiCommand::ACTION_REALITY_SCAN,
            'payload' => ['targets' => array_values($targets)],
            'status' => NodeApiCommand::STATUS_PENDING,
        ]);

        $deadline = microtime(true) + max(0.0, (float) $this->option('wait'));

        while (microtime(true) < $deadline) {
            $fresh = $command->fresh();

            if ($fresh->status === NodeApiCommand::STATUS_FAILED) {
                $output = (string) $fresh->result;
                if (! str_contains($output, 'unknown action')) {
                    $this->warn("{$node->name}：扫描失败 " . $output);
                }

                return null;
            }

            if ($fresh->status === NodeApiCommand::STATUS_SUCCESS) {
                $rows = json_decode((string) $fresh->result, true);
                if (! is_array($rows)) {
                    return null;
                }

                $map = [];
                foreach ($rows as $row) {
                    if (! is_array($row) || ! isset($row['target'])) {
                        continue;
                    }
                    // 节点侧会给裸域名补 :443，这里按主机名对齐（忽略端口差异）
                    $key = XrayConfigService::hostOf((string) $row['target']);
                    if ($key !== '') {
                        $map[$key] = $row;
                    }
                }

                return $map;
            }

            usleep(300_000);
        }

        return null;
    }

    /** 写操作日志（后台「日志」页可见）；同一问题 24h 内只留一条，避免每天刷屏。 */
    private function record(string $message): void
    {
        $exists = OperationLog::where('type', 'system')
            ->where('action', 'REALITY 巡检')
            ->where('error', $message)
            ->where('created_at', '>=', now()->subDay())
            ->exists();

        if ($exists) {
            return;
        }

        OperationLog::create([
            'type' => 'system',
            'actor_name' => 'reality-audit',
            'action' => 'REALITY 巡检',
            'method' => 'CLI',
            'path' => 'xray:reality-audit',
            'status' => 500,
            'error' => $message,
            'created_at' => now(),
        ]);
    }
}
