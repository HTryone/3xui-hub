<?php

namespace App\Console\Commands;

use App\Drivers\NodeDriverFactory;
use App\Models\Node;
use App\Models\User;
use App\Services\BanService;
use Illuminate\Console\Command;

/**
 * Re-enable clients that exist on a 3x-ui node but are switched off there while
 * the panel still treats them as active. See issue #51: the panel never reads
 * the node-side enable flag back, so a wrongly disabled client stays dark.
 */
class ReconcileClientEnableCommand extends Command
{
    protected $signature = 'users:reconcile-enable {--dry-run : list only} {--node= : limit to one node id}';

    protected $description = 'Reconcile: re-enable clients disabled on nodes while the panel still allows them';

    public function handle(NodeDriverFactory $factory, BanService $ban): int
    {
        $dry = (bool) $this->option('dry-run');
        $nowMs = (int) round(microtime(true) * 1000);
        $checked = 0;
        $fixed = 0;
        $failed = 0;
        $skipped = 0;

        $nodes = Node::where('enabled', true)->get()->filter(fn ($n) => ! $n->isXray());
        if ($id = $this->option('node')) {
            $nodes = $nodes->where('id', (int) $id);
        }

        $query = User::query()
            ->whereNotNull('plan_id')
            ->where('enabled', true)
            ->whereNull('traffic_disabled_at')
            ->where(fn ($q) => $q->whereNull('expired_at')->orWhere('expired_at', '>', now()))
            ->where(fn ($q) => $q->where('traffic_limit', 0)->orWhereColumn('traffic_used', '<', 'traffic_limit'))
            ->orderBy('id');

        $query->chunkById(200, function ($users) use ($factory, $ban, $nodes, $dry, $nowMs, &$checked, &$fixed, &$failed, &$skipped) {
            foreach ($nodes as $node) {
                $driver = $factory->make($node);
                foreach ($users as $user) {
                    $email = $user->clientEmail();
                    try {
                        $traffic = $driver->getClientTraffic($email);
                    } catch (\Throwable $e) {
                        continue;
                    }
                    if (! is_array($traffic)) {
                        continue;
                    }
                    $checked++;
                    if (($traffic['enable'] ?? null) !== false) {
                        continue;
                    }
                    $used = (int) ($traffic['up'] ?? 0) + (int) ($traffic['down'] ?? 0);
                    $total = (int) ($traffic['total'] ?? 0);
                    $expiry = (int) ($traffic['expiryTime'] ?? 0);
                    if ($total > 0 && $used >= $total) {
                        $skipped++;
                        continue;
                    }
                    if ($expiry > 0 && $expiry <= $nowMs) {
                        $skipped++;
                        continue;
                    }
                    $this->warn(($dry ? '[dry] ' : '') . "{$node->name}(#{$node->id}) {$email} disabled on node -> re-enable");
                    if ($dry) {
                        $fixed++;
                        continue;
                    }
                    try {
                        // toggleClient 现在只有「全部启用节点确认」才算成功。
                        // 以前忽略返回值直接计成功，会导致命令报 re-enabled=N
                        // 而节点上的 client 实际仍关闭（2026-10-23 事故）。
                        if ($ban->toggleClient($user, true)) {
                            $fixed++;
                        } else {
                            $failed++;
                            $this->error("{$node->name} {$email}: 未全部节点确认，下一轮重试");
                        }
                    } catch (\Throwable $e) {
                        $failed++;
                        $this->error("{$node->name} {$email}: " . $e->getMessage());
                    }
                }
            }
        });

        $this->info(($dry ? '[dry-run] ' : '') . "checked={$checked} re-enabled={$fixed} failed={$failed} skipped_over_or_expired={$skipped}");

        return self::SUCCESS;
    }
}
