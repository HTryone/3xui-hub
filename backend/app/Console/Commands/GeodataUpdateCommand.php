<?php

namespace App\Console\Commands;

use App\Services\GeodataService;
use Illuminate\Console\Command;

/**
 * Geo 数据更新命令（issue #40；每周定时任务与手动补跑共用）。
 *
 *   php artisan geodata:update            只检查并下载（不发节点）
 *   php artisan geodata:update --dispatch 有更新时自动分发到所有 enabled xray 节点
 */
class GeodataUpdateCommand extends Command
{
    protected $signature = 'geodata:update
        {--dispatch : 有文件更新时自动分发到所有 enabled 的 xray 节点}
        {--force-dispatch : 不管有没有更新，都分发面板当前文件（补推失败节点用）}';

    protected $description = 'Geo 数据（geoip.dat / geosite.dat）更新：从上游白名单下载，可自动分发到节点';

    public function handle(GeodataService $geo): int
    {
        $results = $geo->update();
        $updated = 0;

        foreach ($results as $name => $r) {
            $detail = $r['message'] ?? ($r['size'] ?? 0) . ' 字节';
            if ($r['status'] === 'updated') {
                $updated++;
                $this->info(sprintf('%s：已更新（%s）', $name, $detail));
            } else {
                $this->line(sprintf('%s：%s（%s）', $name, $r['status'], $detail));
            }
        }

        $shouldDispatch = $this->option('force-dispatch') || ($this->option('dispatch') && $updated > 0);

        if ($shouldDispatch) {
            $res = $geo->dispatchToNodes();
            if ($res['missing']) {
                $this->warn('面板上没有 geo 文件，跳过分发。');

                return self::SUCCESS;
            }
            $this->info(sprintf('已分发到 %d 个节点（跳过 %d 个已有在途任务的节点）。', $res['queued'], $res['skipped']));
        } else {
            $this->line($updated > 0 ? '（未分发；如需推送节点请加 --dispatch）' : '无更新。');
        }

        return self::SUCCESS;
    }
}
