<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\OperationLog;
use App\Traits\ApiResponse;
use Illuminate\Console\Scheduling\Schedule;
use Illuminate\Support\Facades\Cache;
use Illuminate\Support\Facades\DB;

/**
 * 系统状态：操作日志 + 定时任务 + worker 心跳。
 * GET /admin-api/system/status
 */
class SystemStatusController extends Controller
{
    use ApiResponse;

    public function status(): \Illuminate\Http\JsonResponse
    {
        // 操作日志：最近 100 条（按 id 倒序 = 时间倒序）
        $logs = OperationLog::query()
            ->orderByDesc('id')
            ->limit(100)
            ->get(['id', 'type', 'actor_name', 'action', 'method', 'path', 'status', 'error', 'created_at']);

        return $this->success([
            'logs'   => $logs,
            'tasks'  => $this->taskStatuses(),
            'worker' => $this->workerStatus(),
        ]);
    }

    /**
     * 定时任务状态：所有 Schedule 事件 + 每个任务最近一次运行记录。
     */
    protected function taskStatuses(): array
    {
        /** @var Schedule $schedule */
        $schedule = app(Schedule::class);

        $events = [];
        foreach ($schedule->events() as $event) {
            $command = $event->command ?? '';
            // 匹配键与监听器一致：优先任务名（description），否则取命令短名
            $key = $event->description ?? $this->shortCommand($command);

            $last = $key
                ? DB::table('scheduled_task_runs')
                    ->where('command', $key)
                    ->orderByDesc('id')
                    ->first(['status', 'error', 'duration_ms', 'ran_at'])
                : null;

            $events[] = [
                'command'      => $key,
                'expression'   => $event->getExpression(),
                'next_run'     => $event->nextRunDate()?->toDateTimeString(),
                'last_run'     => $last?->ran_at,
                'last_status'  => $last?->status,
                'last_error'   => $last?->error,
                'last_dur_ms'  => $last?->duration_ms,
                'has_mutex'    => !empty($event->mutex),
            ];
        }

        // 最近 10 条运行记录（含报错）
        $recent = DB::table('scheduled_task_runs')
            ->orderByDesc('id')
            ->limit(10)
            ->get(['command', 'status', 'error', 'duration_ms', 'ran_at']);

        return ['events' => $events, 'recent' => $recent];
    }

    /**
     * 提取可读命令名：`php F:\...\artisan traffic:sync` → `traffic:sync`。
     */
    protected function shortCommand(string $command): string
    {
        if ($command === '') {
            return '';
        }
        if (str_contains($command, 'artisan ')) {
            return trim(explode('artisan ', $command)[1]);
        }
        return $command;
    }

    /**
     * worker 状态：心跳 + 队列积压 + 失败任务。
     * 心跳由 BanCheckJob（每 5 分钟）写入，超过 15 分钟视为不在线。
     */
    protected function workerStatus(): array
    {
        $heartbeat = Cache::get('controlhub:worker-heartbeat');
        $lastSeen = is_string($heartbeat) ? \Illuminate\Support\Carbon::parse($heartbeat) : $heartbeat;
        $alive = $lastSeen instanceof \Illuminate\Support\Carbon
            && now()->diffInMinutes($lastSeen) < 15;

        $pending = $running = $failed = 0;
        $recentFailed = collect();
        try {
            $pending = DB::table('jobs')->whereNull('reserved_at')->count();
            $running = DB::table('jobs')->whereNotNull('reserved_at')->count();
            $failed  = DB::table('failed_jobs')->count();
            $recentFailed = DB::table('failed_jobs')
                ->orderByDesc('id')
                ->limit(5)
                ->get(['queue', 'name', 'attempts', 'failed_at']);
        } catch (\Throwable) {
            // 队列表未就绪时返回 0
        }

        return [
            'alive'         => $alive,
            'last_seen'     => $lastSeen ? $lastSeen->toDateTimeString() : null,
            'pending'       => $pending,
            'running'       => $running,
            'failed_count'  => $failed,
            'recent_failed' => $recentFailed,
        ];
    }
}
