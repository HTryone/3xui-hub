<?php

namespace App\Console\Commands;

use App\Drivers\Xray\OutboundSubscriptionService;
use App\Models\XrayOutboundSubscription;
use Illuminate\Console\Command;
use Illuminate\Support\Facades\Log;

/**
 * 出站订阅定时刷新（站点调度每 5 分钟触发）。
 *
 * 逐条检查到期项（last_fetched_at + update_interval ≤ now）→ 拉取 → 解析 →
 * 标签稳定 → 缓存；内容变化则 bump 节点 config_version（节点秒级拉新）。
 * 单条失败保留上次成功缓存，不阻断其它订阅。
 */
class RefreshOutboundSubscriptions extends Command
{
    protected $signature = 'xray:refresh-outbound-subs';

    protected $description = '刷新到期的出站订阅（服务器池），内容变化则 bump 节点 config_version';

    public function handle(OutboundSubscriptionService $service): int
    {
        $enabled = XrayOutboundSubscription::where('enabled', true)->count();
        if ($enabled === 0) {
            $this->info('没有启用的出站订阅，跳过');

            return self::SUCCESS;
        }

        $this->info(now()->format('Y-m-d H:i:s') . " 检查 {$enabled} 条出站订阅...");

        try {
            $refreshed = $service->refreshDue();
        } catch (\Throwable $e) {
            Log::warning('出站订阅定时刷新失败', ['error' => $e->getMessage()]);
            $this->error('刷新异常：' . $e->getMessage());

            return self::FAILURE;
        }

        $this->info("完成：已刷新 {$refreshed} 条");

        return self::SUCCESS;
    }
}
