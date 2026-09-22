<?php

namespace App\Jobs;

use App\Models\User;
use App\Services\BanService;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;

/**
 * 流量检查 Job（M8.3）：遍历启用中的用户，满足超量/到期 → 关闭 3x-ui 流量。
 * 不封禁用户，用户仍能登录。
 */
class BanCheckJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public function handle(BanService $banService): void
    {
        // worker 心跳：被 worker 消费即视为在线（每 5 分钟一次），存字符串避免序列化问题
        \Illuminate\Support\Facades\Cache::put('controlhub:worker-heartbeat', now()->toDateTimeString(), now()->addMinutes(20));

        User::where('enabled', true)
            ->with('plan')
            ->each(function (User $user) use ($banService) {
                // needsDisable：命中关闭条件且【未处于已确认关闭的时效内】才发请求。
                // 已关闭未超时效 → 直接跳过（0 次 HTTP），避免每 5 分钟把同一批用户重关一遍；
                // 超时效（config('ban.recheck_after_hours')）→ 照旧重新校验一次，兜住面板侧漂移。
                if ($banService->needsDisable($user)) {
                    $banService->toggleClient($user, false);
                }
            });
    }
}
