<?php

namespace App\Console\Commands;

use App\Drivers\NodeDriverFactory;
use App\Models\Node;
use App\Models\User;
use App\Services\BanService;
use App\Services\NodeConcurrency;
use App\Services\ThreeXUi\ThreeXUiClient;
use App\Services\ThreeXUiClientFactory;
use App\Services\TrafficSyncService;
use GuzzleHttp\Promise\PromiseInterface;
use GuzzleHttp\Promise\Utils;
use Illuminate\Console\Command;

/**
 * 流量自动同步命令（批量优化版）。
 * 每个节点1次HTTP拉取全量流量，并行请求后批量写入。
 *
 * 运行方式：php artisan traffic:sync
 * 定时调度：routes/console.php 每5分钟
 */
class SyncTrafficCommand extends Command
{
    protected $signature = 'traffic:sync';
    protected $description = '自动同步所有用户流量并关停超限用户';

    public function handle(
        NodeDriverFactory $driverFactory,
        TrafficSyncService $syncService,
        BanService $banService,
        ThreeXUiClientFactory $clientFactory,
    ): int {
        $this->info('[' . now()->format('Y-m-d H:i:s') . '] 开始流量同步...');

        $nodes = Node::where('enabled', true)->get();
        if ($nodes->isEmpty()) {
            $this->info('无启用节点');
            return self::SUCCESS;
        }

        // 先把能并发拉取的节点流量一次性并发拉回（每节点 1 次 listInbounds），
        // 再按原顺序逐节点落库 —— 落库顺序、单节点失败互不影响等语义与纯串行版一致，
        // 只是把「N 次串行等待」压成「ceil(N/并发) 次并发等待」（issue #42）。
        $prefetched = $this->prefetchStats($nodes, $clientFactory);

        $totalSynced = 0;
        $allDeltaUserIds = [];

        foreach ($nodes as $node) {
            // xray 节点流量由 agent 主动推送（/node-api/push），不做面板侧轮询
            if ($node->isXray()) {
                continue;
            }

            try {
                $result = $syncService->syncNodeFromSource($node, function () use ($prefetched, $driverFactory, $node) {
                    if (array_key_exists($node->id, $prefetched)) {
                        $stats = $prefetched[$node->id];
                        if ($stats instanceof \Throwable) {
                            throw $stats; // 与串行版「取数失败 → 跳过该节点」同一个出口
                        }

                        return $stats;
                    }

                    $driver = $driverFactory->make($node);

                    // 数据层已按 email 去重（同 email 跨入站只取首值，避免 N 倍计费）
                    return $driver->getClientStatsByEmail();
                });
            } catch (\Throwable) {
                continue;
            }
            if (!$result['acquired']) continue;

            $totalSynced += count($result['deltaMap']);
            $allDeltaUserIds = array_merge($allDeltaUserIds, array_keys($result['deltaMap']));

            $this->line("  节点 {$node->name}: " . count($result['snapshotData']) . " 用户, " . count($result['deltaMap']) . " 有增量");
        }

        // 3. Ban检查（仅检查有流量变化的用户）
        $banned = 0;
        if (!empty($allDeltaUserIds)) {
            $users = User::whereIn('id', array_unique($allDeltaUserIds))->with('plan')->get();
            foreach ($users as $user) {
                $fresh = $user->fresh();
                $fresh->load('plan');
                $reason = $banService->banReason($fresh);
                if ($reason === false || !$fresh->enabled) {
                    continue;
                }

                // 已确认关闭且未超时效 → 跳过（0 次 HTTP）。
                // 同时也消掉了「同一轮里既被本命令关、又被 BanCheckJob 关」的重复关闭。
                if ($banService->isRecentlyDisabled($fresh)) {
                    continue;
                }

                $banService->toggleClient($fresh, false);
                $banned++;
                $this->line("  关闭流量 #{$fresh->id} ({$fresh->email}): {$reason}");
            }
        }

        $this->info("同步完成: {$totalSynced} 条用户节点记录有增量, {$banned} 关停");
        return self::SUCCESS;
    }

    /**
     * 并发拉取各节点的 client 流量（每节点 1 次 listInbounds）。
     *
     * 只拉「3x-ui + api_key（Bearer）」节点：cookie 模式要维护登录会话不能并发，
     * 它们留给后面的串行路径按原样取数（不进这个数组）。
     *
     * 返回 [node_id => array|Throwable]：
     * - 正常是 [email => ['up'=>int,'down'=>int]]；
     * - 该节点取数失败则放异常对象，落库阶段抛出去 → 与串行版一样「跳过该节点」。
     *
     * 只做「取数」，不写库：DB 写入仍在下面按节点顺序进行，避免并发写让快照/增量互相干扰。
     *
     * @param  \Illuminate\Support\Collection<int,Node>  $nodes
     * @return array<int,array|\Throwable>
     */
    private function prefetchStats($nodes, ThreeXUiClientFactory $clientFactory): array
    {
        $targets = [];
        foreach ($nodes as $node) {
            if ($node->isXray() || ! NodeConcurrency::eligible($node)) {
                continue;
            }
            $targets[] = $node;
        }

        $stats = [];

        foreach (array_chunk($targets, NodeConcurrency::limit()) as $batch) {
            $promises = [];
            // 同一批共用一个传输层才是真并发（各用各的 curl_multi 只能逐个 wait）
            $transport = ThreeXUiClient::newSharedTransport();

            foreach ($batch as $node) {
                try {
                    $client = $clientFactory->forNode($node);
                    $client->useSharedTransport($transport);
                    $promises[$node->id] = $client->clientStatsAsync();
                } catch (\Throwable $e) {
                    $stats[$node->id] = $e; // 该节点取数失败，其它节点照常
                }
            }

            if ($promises === []) {
                continue;
            }

            foreach (Utils::settle($promises)->wait() as $nodeId => $outcome) {
                if (($outcome['state'] ?? null) === PromiseInterface::FULFILLED) {
                    $stats[$nodeId] = is_array($outcome['value'] ?? null) ? $outcome['value'] : [];

                    continue;
                }

                $reason = $outcome['reason'] ?? 'stats failed';
                $stats[$nodeId] = $reason instanceof \Throwable
                    ? $reason
                    : new \RuntimeException((string) $reason);
            }
        }

        return $stats;
    }
}
