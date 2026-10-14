<?php

namespace App\Console\Commands;

use App\Services\MonitorService;
use Illuminate\Console\Command;

/**
 * 机器流量降采样：把超过保留期（config('monitor.retain_days')，默认 90 天）的
 * L0 采样折成 monitor_daily 的「当天增量」后删除原始样本。
 *
 * 每天 04:30 调度（避开 03:30 的数据库备份）。幂等：同日重复运行结果一致
 * （upsert 覆盖而非累加，且只处理仍有过期样本的天）—— 详见 MonitorService::rollup()。
 */
class MonitorRollupCommand extends Command
{
    protected $signature = 'monitor:rollup';

    protected $description = '把超过保留期的机器流量采样折算进天聚合表并删除原始样本';

    public function handle(MonitorService $monitor): int
    {
        $result = $monitor->rollup();

        $this->info(sprintf(
            '完成：删除 %d 条过期样本，覆盖 %d 个 (节点, 日期) 分组（%d 个节点）',
            $result['samples'],
            $result['groups'],
            $result['nodes'],
        ));

        return self::SUCCESS;
    }
}
