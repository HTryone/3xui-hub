<?php

namespace App\Jobs;

use App\Models\MailLog;
use App\Models\SiteConfig;
use App\Models\User;
use App\Services\MailNotifyService;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;
use Illuminate\Support\Facades\Cache;

/**
 * 扫描类通知：流量即将用尽 / 套餐即将到期 / 已到期 / 无套餐。
 *
 * 这几类不能挂在某个同步动作上（流量用尽能挂BanService，但「即将」是阈值判断，
 * 「无套餐」没有事件），所以由定时任务扫描。
 *
 * 防重复发信：靠 Cache 标记（入队那一刻就落），不用 mail_logs ——后者要等
 * 真正发出才写入，而本任务每 5 分钟一轮，队列未跑完时会重复发信。
 * 同一天同一场景只发一次，避免把用户刷屏。
 */
class MailNotifyScanJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public function handle(): void
    {
        $this->scanTrafficAlmost();
        $this->scanExpiring();
        $this->scanExpired();
        $this->scanNoPlan();
    }

    /** 流量即将用尽（默认 90%，管理员可调） */
    private function scanTrafficAlmost(): void
    {
        if (!MailNotifyService::isEnabled('traffic_almost')) return;

        $threshold = (int) (SiteConfig::getValue('notify_traffic_almost_threshold') ?: 90);
        if ($threshold <= 0 || $threshold > 100) $threshold = 90;

        // 用量达到 limit 的 threshold%，但还没用尽（用尽那一路挂在 BanService 上）
        // whereColumn 不接受闭包，阈值直接算成 traffic_used >= traffic_limit * (pct/100)
        $ratio = $threshold / 100;

        User::where('enabled', true)
            ->where('traffic_limit', '>', 0)
            ->whereRaw('traffic_used >= FLOOR(traffic_limit * ?)', [$ratio])
            ->whereColumn('traffic_used', '<', 'traffic_limit')
            ->with('plan')
            ->chunkById(200, function ($users) {
                foreach ($users as $user) {
                    $this->dispatchOnce('traffic_almost', $user);
                }
            });
    }

    /** 套餐即将到期（默认提前 3 天，管理员可调） */
    private function scanExpiring(): void
    {
        if (!MailNotifyService::isEnabled('expiring')) return;

        $days = (int) (SiteConfig::getValue('notify_expiring_days') ?: 3);

        User::where('enabled', true)->whereNotNull('expired_at')
            ->where('expired_at', '>', now())
            ->where('expired_at', '<=', now()->addDays($days))
            ->with('plan')
            ->chunkById(200, function ($users) {
                foreach ($users as $user) {
                    $this->dispatchOnce('expiring', $user);
                }
            });
    }

    /** 已到期（过期后的 7 天内提醒一次） */
    private function scanExpired(): void
    {
        if (!MailNotifyService::isEnabled('expired')) return;

        User::where('enabled', true)->whereNotNull('expired_at')
            ->where('expired_at', '<=', now())
            ->where('expired_at', '>=', now()->subDays(7))
            ->with('plan')
            ->chunkById(200, function ($users) {
                foreach ($users as $user) {
                    $this->dispatchOnce('expired', $user);
                }
            });
    }

    /** 无套餐 */
    private function scanNoPlan(): void
    {
        if (!MailNotifyService::isEnabled('no_plan')) return;

        User::where('enabled', true)->whereNull('plan_id')
            ->chunkById(200, function ($users) {
                foreach ($users as $user) {
                    $this->dispatchOnce('no_plan', $user);
                }
            });
    }

    /**
     * 同一天同一场景对同一用户只发一次。
     *
     * 防重靠 Cache 而不是 mail_logs：mail_logs 是真正发出后才写的，
     * 而本任务是每 5 分钟一轮 —— 两轮之间队列还没跑完的话，
     * 用 mail_logs 判断会把同一封发两遍。Cache 标记在入队那一刻就落。
     */
    private function dispatchOnce(string $scene, User $user): void
    {
        if (empty($user->email)) return;

        // 先渲染 + 解析收件人，确认这封真的能发，再落防重标记。
        // 顺序反过来的话：管理员没配收件邮箱时标记已经写进 Cache，
        // 之后补上邮箱这一整天都不会再发了。
        $rendered = MailNotifyService::render($scene, $user->loadMissing('plan'));
        if ($rendered === []) return;

        $to = $rendered['to_type'] === 'admin'
            ? SiteConfig::getValue('notify_admin_email')
            : $user->email;

        if (empty($to)) return;

        // Cache::add 返回 true = 本次是首次写入（标记新建成功），继续发；
        // 返回 false = 标记已存在（今天已入队过），跳过。
        $key = "mail_notify:{$scene}:{$user->id}:" . today()->toDateString();
        if (!Cache::add($key, 1, now()->endOfDay())) {
            return;
        }

        SendMailJob::dispatch(
            toEmail: $to,
            subject: $rendered['subject'],
            htmlBody: $rendered['body'],
            type: MailLog::TYPE_NOTIFY,
            userId: $user->id,
            scene: $scene,
        );
    }
}