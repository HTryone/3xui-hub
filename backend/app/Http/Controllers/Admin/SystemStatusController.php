<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\OperationLog;
use App\Traits\ApiResponse;
use Illuminate\Console\Scheduling\Schedule;
use Illuminate\Support\Carbon;
use Illuminate\Support\Facades\Cache;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Log;
use Illuminate\Support\Str;

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
                'next_run'     => $event->nextRunDate()?->toISOString(),
                'last_run'     => $this->isoTime($last?->ran_at),
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
            ->get(['command', 'status', 'error', 'duration_ms', 'ran_at'])
            ->map(fn ($row) => [
                'command'     => $row->command,
                'status'      => $row->status,
                'error'       => $row->error,
                'duration_ms' => $row->duration_ms,
                'ran_at'      => $this->isoTime($row->ran_at),
            ])
            ->all();

        return ['events' => $events, 'recent' => $recent];
    }

    /**
     * DB::table 取出的时间列是裸字符串（无 cast），统一转成 ISO8601 带 Z（App 时区 = UTC）。
     * 用 toISOString() 而非 toIso8601String()：后者产出 "+00:00" 后缀，而前者与 Eloquent 对
     * created_at 的序列化结果完全一致（…T01:00:00.000000Z），前端才能对所有时间字段统一解析。
     * 空值保持 null，前端按占位符处理。
     */
    protected function isoTime(?string $value): ?string
    {
        return $value ? Carbon::parse($value)->toISOString() : null;
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
     * 从 failed_jobs.payload 里取任务名（Laravel 序列化 payload 的 displayName）。
     * payload 是外部写入的裸 JSON，任何解析失败都只能兜底成 null——绝不能抛，
     * 否则一条坏记录就能让整个「最近失败任务」区块消失。
     */
    protected function jobNameFromPayload(?string $payload): ?string
    {
        if (!is_string($payload) || $payload === '') {
            return null;
        }

        try {
            $decoded = json_decode($payload, true, 512, JSON_THROW_ON_ERROR);
        } catch (\Throwable) {
            return null;
        }

        $name = is_array($decoded) ? ($decoded['displayName'] ?? null) : null;

        return is_string($name) && $name !== '' ? $name : null;
    }

    /**
     * 异常首行摘要，形如 `RuntimeException: boom in /path/Foo.php:21`。
     * 去掉尾部的绝对路径+行号：既降噪，也避免把服务器路径暴露到管理页。
     */
    protected function errorSummary(?string $exception): ?string
    {
        if (!is_string($exception) || trim($exception) === '') {
            return null;
        }

        // Str::before 取首个 "\n" 之前的内容，\r\n 的尾 \r 交给 trim
        $firstLine = trim(Str::before($exception, "\n"));
        if ($firstLine === '') {
            return null;
        }

        return Str::limit(preg_replace('/\s+in\s+[^\s]*:\d+$/', '', $firstLine) ?? $firstLine, 300);
    }

    /**
     * worker 状态：心跳 + 队列积压 + 失败任务。
     * 心跳由 BanCheckJob（每 5 分钟）写入，超过 15 分钟视为不在线。
     */
    protected function workerStatus(): array
    {
        $heartbeat = Cache::get('controlhub:worker-heartbeat');
        $lastSeen = is_string($heartbeat) ? Carbon::parse($heartbeat) : $heartbeat;
        $alive = $lastSeen instanceof Carbon
            && now()->diffInMinutes($lastSeen) < 15;

        $pending = $running = $failed = 0;
        $recentFailed = collect();
        try {
            $pending = DB::table('jobs')->whereNull('reserved_at')->count();
            $running = DB::table('jobs')->whereNotNull('reserved_at')->count();
            $failed  = DB::table('failed_jobs')->count();

            // failed_jobs 实际只有 id/uuid/connection/queue/payload/exception/failed_at：
            // 任务名要从 payload 里取，不要在表上直接 select 不存在的列。
            $recentFailed = DB::table('failed_jobs')
                ->orderByDesc('id')
                ->limit(5)
                ->get(['id', 'queue', 'payload', 'exception', 'failed_at'])
                ->map(fn ($row) => [
                    'queue'         => $row->queue,
                    'name'          => $this->jobNameFromPayload($row->payload),
                    // failed_jobs 无 attempts 列，payload 里也没有该键（已用真实失败记录核实：
                    // Laravel 只在 release() 时改 payload 的 attempts，而失败落库的 payload
                    // 是 dispatch 时的原始快照）。拿不到就返回 null，不伪造 0。
                    'attempts'      => null,
                    'error_summary' => $this->errorSummary($row->exception),
                    'failed_at'     => $this->isoTime($row->failed_at),
                ])
                ->all();
        } catch (\Throwable $e) {
            // 队列表未就绪（迁移未跑等）时保持 0 / 空数组的容错语义，
            // 但必须留痕：正是这里的静默吞异常让 recent_failed 空了都没人发现。
            Log::warning('系统状态：读取队列数据失败', [
                'exception' => $e::class,
                'error'     => $e->getMessage(),
                'at'        => $e->getFile() . ':' . $e->getLine(),
            ]);
        }

        return [
            'alive'         => $alive,
            'last_seen'     => $lastSeen ? $lastSeen->toISOString() : null,
            'pending'       => $pending,
            'running'       => $running,
            'failed_count'  => $failed,
            'recent_failed' => $recentFailed,
        ];
    }
}
