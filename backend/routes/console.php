<?php

use App\Jobs\BanCheckJob;
use App\Jobs\HealthCheckJob;
use App\Jobs\MailNotifyScanJob;
use App\Jobs\MailScheduleJob;
use Illuminate\Support\Facades\Schedule;

// 流量自动同步：每5分钟
Schedule::command('traffic:sync')->everyFiveMinutes()->name('traffic-sync')->withoutOverlapping();

// 全量封禁扫描：每5分钟遍历启用用户（到期/超量 → 关闭 3x-ui 流量，不封禁账号）。
// 不依赖流量增量，兜底关闭「耗尽后停止传输」的长期套餐用户（增量门控检查漏掉的场景）。
Schedule::job(BanCheckJob::class)->everyFiveMinutes()->name('ban-check')->withoutOverlapping();

// 节点健康检查：每5分钟遍历 enabled 节点刷新 status/latency。
// 没有它节点 status 一旦变 offline 就只能靠管理员手点「测试连接」恢复（订阅要求 status=online）。
Schedule::job(HealthCheckJob::class)->everyFiveMinutes()->name('health-check')->withoutOverlapping();

// 每天检查并重置周期套餐月流量（基于用户的 next_traffic_reset_at）
Schedule::command('traffic:monthly-reset')->dailyAt('00:00')->name('monthly-reset-traffic')->withoutOverlapping();

// 每天刷新到期的邀请码（旧码立即失效，只认最新码）
Schedule::command('invite:refresh')->dailyAt('00:10')->name('invite-code-refresh')->withoutOverlapping();

// 数据库每日备份：03:30 导出到 config('backup.dir')（默认 /var/backups/3xui-hub），
// 保留最新 config('backup.keep') 份（默认 7）。选 03:30 是避开 00:00-00:10 的
// 月流量重置 / 邀请码刷新与整点前后的业务高峰；dump 走 --single-transaction 不锁表。
Schedule::command('backup:run')->dailyAt('03:30')->name('db-backup')->withoutOverlapping();

// 机器流量降采样：把超过保留期（config('monitor.retain_days')，默认 90 天）的 L0 采样
// 折算进 monitor_daily 的当天增量后删除原始样本 —— 采样表不随时间无界增长。
// 排在 04:30（备份 03:30 之后一个整点），避免和每日备份抢 IO；命令本身幂等，重跑无副作用。
Schedule::command('monitor:rollup')->dailyAt('04:30')->name('monitor-rollup')->withoutOverlapping();

// 作废超时（1 小时）未支付的订单，防止 pending 单无限堆积：
// 带码死单会被反复复用导致用户永远付不了款；无码死单会被反复重发网关刷 ERROR 日志。
// TTL 显式传参，不依赖命令默认值 —— 默认值一旦被改动，这里的口径会跟着变，容易踩坑。
Schedule::command('order:expire-pending', ['--hours' => 1])->hourly()->name('expire-pending-orders')->withoutOverlapping();

// 任务超时兜底：超过阈值（config('tasks.stale_after_minutes')）未终态的异步任务标记失败，防止 running 永久停留
Schedule::call(fn () => app(\App\Services\AsyncTaskService::class)->timeoutStale())->everyFiveMinutes()->name('async-task-timeout')->withoutOverlapping();

// 邮件自动通知扫描：每 5 分钟检查「流量即将用尽 / 即将到期 / 已到期」。
// 四个开关默认关闭（SiteConfig 未配置即关闭）；调度条件：四开关全关时任务整体跳过、不空转。
// 同一天同一场景对同一用户只发一次，防重复逻辑在 MailNotifyScanJob 内。
Schedule::job(MailNotifyScanJob::class)->everyFiveMinutes()->name('mail-notify-scan')->withoutOverlapping()
    ->when(fn () => \App\Services\MailNotifyService::anyEnabled());

// 按月定时发信：每 5 分钟检查「管理员配置的每月第 N 天 HH:MM」是否到点，
// 到点且当月没发过就把配好的那封信入队（收件人/标题/正文/限速复用批量发信配置）。
// 默认关闭（SiteConfig 未配置即关）；未开启时任务整体跳过；同一自然月只发一轮（MailScheduleJob 内去重）。
Schedule::job(MailScheduleJob::class)->everyFiveMinutes()->name('mail-schedule-scan')->withoutOverlapping()
    ->when(fn () => \App\Services\MailScheduleService::isEnabled());

// 第三方订阅拉取：每 2 小时抓取启用的第三方订阅并缓存节点链接。
// 调度条件：总开关开启且有启用订阅才跑；拉取失败保留上次成功缓存（ThirdPartyService 内处理）。
Schedule::command('third-party:fetch')->everyTwoHours()->name('third-party-fetch')->withoutOverlapping()
    ->when(fn () => \App\Models\SiteConfig::getValue('third_party_enabled', '0') === '1'
        && \App\Models\ThirdPartySub::query()->where('enabled', true)->exists());

// WARP 定时换 IP：每 5 分钟检查各节点 WARP 账户的 auto_rotate_hours 是否到点（0=关闭）。
// 调度条件：没有任何「已启用 + 开启定时换 IP」的账户时，任务整体跳过 —— 没启动 WARP 不产生空进程。
// 换 IP 是小时级需求，5 分钟精度足够。到点换 IP = 重新注册设备 → 重组 wireguard 出站 → bump config。
Schedule::command('xray:warp-rotate')->everyFiveMinutes()->name('xray-warp-rotate')->withoutOverlapping()
    ->when(fn () => \App\Models\NodeWarpAccount::query()
        ->where('auto_rotate_hours', '>', 0)
        ->where('enabled', true)
        ->exists());

// 出站订阅刷新：每 5 分钟检查各节点订阅是否到期（last_fetched_at + update_interval）。
// 调度条件：存在启用订阅才跑；内容变化则 bump 节点 config_version → 节点秒级拉新。
Schedule::command('xray:refresh-outbound-subs')->everyFiveMinutes()->name('xray-refresh-outbound-subs')->withoutOverlapping()
    ->when(fn () => \App\Models\XrayOutboundSubscription::query()->where('enabled', true)->exists());

// Geo 数据更新（issue #40）：每周一凌晨从上游白名单检查 geoip.dat / geosite.dat，
// 有更新则下载并分发到所有 enabled xray 节点（节点侧 sha 相同则跳过，不打扰存量）。
// 放在 04:45：错开 04:30 的 monitor:rollup；周一低频，geo 数据本身也不是每日变更。
Schedule::command('geodata:update', ['--dispatch' => true])->weeklyOn(1, '04:45')->name('geodata-update')->withoutOverlapping();

// 节点侧「已关闭但面板仍放行」的对账恢复（issue #51）。
// 每 5 分钟扫一遍 3x-ui 节点：面板认为该开、节点上 enable=false 的 client 重新打开。
// 与「开关是否全部节点确认」配合：toggleClient 全确认才算成功，失败的下轮由本命令重试。
Schedule::command('users:reconcile-enable')->everyFiveMinutes()->name('users-reconcile-enable')->withoutOverlapping();
