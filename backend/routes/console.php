<?php

use App\Jobs\BanCheckJob;
use Illuminate\Support\Facades\Schedule;

// 流量自动同步：每5分钟
Schedule::command('traffic:sync')->everyFiveMinutes()->name('traffic-sync')->withoutOverlapping();

// 每天检查并重置周期套餐月流量（基于用户的 next_traffic_reset_at）
Schedule::command('traffic:monthly-reset')->dailyAt('00:00')->name('monthly-reset-traffic')->withoutOverlapping();

// 全量封禁扫描：每5分钟遍历所有启用用户（到期/超量 → 关闭 3x-ui 流量）。
// 不依赖流量增量，兜底关闭「耗尽后停止传输」的长期套餐等用户（BanCheckJob 此前是死代码未调度）。
Schedule::job(BanCheckJob::class)
    ->everyFiveMinutes()
    ->name('ban-check')
    ->withoutOverlapping();
