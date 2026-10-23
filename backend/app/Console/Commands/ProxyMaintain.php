<?php

namespace App\Console\Commands;

use App\Services\ProxyStoreService;
use Illuminate\Console\Command;
use Illuminate\Support\Facades\DB;

/**
 * 代理商城的日常维护。
 *
 * 做三件事：
 *  1. 回采 webshare 子账号用量（只读，展示用；流量上限由上游 proxy_limit 硬控）
 *  2. 到期权益标记 expired，并把对应的上游子账号停用（改随机密码 + 限额压到 0.01GB）
 *  3. 统计输出，便于在日志里核对
 *
 * 不做的事：不主动断共享 socks（那是站长的货源，随时可换）。
 *
 * 运行：php artisan proxy:maintain
 */
class ProxyMaintain extends Command
{
    protected $signature = 'proxy:maintain {--dry-run : 只统计，不回采不停用}';

    protected $description = '代理商城维护：回采用量、到期停用';

    public function handle(ProxyStoreService $service): int
    {
        $dryRun = (bool) $this->option('dry-run');

        $this->info(sprintf(
            '[%s] 库：%s · 模式：%s',
            now()->format('Y-m-d H:i:s'),
            DB::connection()->getDatabaseName(),
            $dryRun ? 'DRY-RUN（只读）' : '写入'
        ));

        if ($dryRun) {
            $this->info('dry-run：跳过实际回采与停用。');

            return self::SUCCESS;
        }

        $stats = $service->maintain();

        $this->info(sprintf(
            '回采成功 %d · 回采失败 %d · 到期权益 %d · 停用账号 %d',
            $stats['pulled'],
            $stats['pull_failed'],
            $stats['expired'],
            $stats['disabled'],
        ));

        return self::SUCCESS;
    }
}
