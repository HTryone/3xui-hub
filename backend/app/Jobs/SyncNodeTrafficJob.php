<?php

namespace App\Jobs;

use App\Drivers\NodeDriverFactory;

use App\Models\Node;
use App\Models\User;
use App\Services\AsyncTaskService;
use App\Services\BanService;
use App\Services\TrafficSyncService;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;

/**
 * 单节点流量同步（批量优化版）。
 * 一次 listInbounds() 拉取全节点 client 流量，内存匹配后批量写入。
 */
class SyncNodeTrafficJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public int $tries = 5;
    public array $backoff = [60, 300, 900, 3600];

    public function __construct(
        public int $nodeId,
        public ?int $taskId = null,
        public ?int $userId = null,
        public ?string $itemKey = null,
    ) {}

    public function handle(
        NodeDriverFactory $driverFactory,
        TrafficSyncService $sync,
        BanService $banService,
        AsyncTaskService $tasks,
    ): void {
        if ($this->taskId !== null && $this->itemKey !== null) {
            if (!$tasks->claimItem($this->taskId, $this->itemKey)) {
                return;
            }
        }

        /** @var Node|null $node */
        $node = Node::find($this->nodeId);
        if (!$node || !$node->enabled) {
            if ($this->taskId !== null && $this->itemKey !== null) {
                $tasks->completeItem($this->taskId, $this->itemKey);
            }
            return;
        }

        try {
            if ($this->userId !== null) {
                $user = User::find($this->userId);
                if (!$user) {
                    if ($this->taskId !== null && $this->itemKey !== null) $tasks->completeItem($this->taskId, $this->itemKey);
                    return;
                }
                $email = $user->clientEmail();
                $result = $sync->syncUserNodeFromSource($user, $node, function () use ($driverFactory, $node, $email) {
                    $driver = $driverFactory->make($node);
                    $stats = $this->mergeStats($driver->getClientStatsGroupedByInbound());
                    return $stats[$email] ?? null;
                });
                $deltaMap = [];
            } else {
                $result = $sync->syncNodeFromSource($node, function () use ($driverFactory, $node) {
                    $driver = $driverFactory->make($node);
                    return $this->mergeStats($driver->getClientStatsGroupedByInbound());
                });
                $deltaMap = $result['deltaMap'];
            }
        } catch (\Throwable $e) {
            throw $e;
        }

        if (!$result['acquired']) {
            $this->release(5);
            return;
        }

        // Ban检查（仅检查有流量变化的用户）
        if (!empty($deltaMap)) {
            $users = User::whereIn('id', array_keys($deltaMap))->with('plan')->get();
            foreach ($users as $user) {
                $fresh = $user->fresh();
                $fresh->load('plan');
                $banService->checkAfterSync($fresh);
            }
        }

        if ($this->taskId !== null && $this->itemKey !== null) {
            $tasks->completeItem($this->taskId, $this->itemKey);
        }
    }

    public function failed(\Throwable $exception): void
    {
        if ($this->taskId !== null && $this->itemKey !== null) {
            report($exception);
            $tasks = app(AsyncTaskService::class);
            $tasks->failItem($this->taskId, $this->itemKey, $tasks->summaryFor('traffic_sync'));
        }
    }

    private function mergeStats(array $statsByInbound): array
    {
        $mergedStats = [];
        foreach ($statsByInbound as $emailStats) {
            foreach ($emailStats as $email => $stat) {
                if (!isset($mergedStats[$email])) {
                    $mergedStats[$email] = ['up' => 0, 'down' => 0];
                }
                $mergedStats[$email]['up'] += $stat['up'];
                $mergedStats[$email]['down'] += $stat['down'];
            }
        }

        return $mergedStats;
    }
}
