<?php

namespace App\Listeners;

use App\Models\ScheduledTaskRun;
use Illuminate\Console\Events\ScheduledTaskFinished;
use Illuminate\Console\Events\ScheduledTaskFailed;
use Illuminate\Console\Scheduling\Event;

/**
 * 记录每个定时任务的一次运行（最近一次，带报错），供系统状态页展示。
 * 每次写入前删除该 command 的历史记录，避免堆积。
 */
class RecordScheduledTaskRun
{
    /** @var int 每条命令保留的运行记录数 */
    private const KEEP = 5;

    public function onFinished(ScheduledTaskFinished $event): void
    {
        $this->record($event->task, 'finished', null, $event->runtime * 1000);
    }

    public function onFailed(ScheduledTaskFailed $event): void
    {
        $this->record($event->task, 'failed', $event->exception->getMessage(), 0);
    }

    private function record(Event $task, string $status, ?string $error, float $durationMs): void
    {
        try {
            // 匹配键：优先任务名（description，如 ban-check），无则回退命令路径。
            // Laravel 13 中 Schedule::job()/Schedule::call() 是 CallbackEvent，$command 为空，
            // 任务名存于 description（由 ->name() 设置），须用它做匹配键。
            $key = $task->description ?? ($task->command ?? '');
            if ($key === '') {
                return;
            }

            ScheduledTaskRun::create([
                'command'      => $key,
                'status'       => $status,
                'error'        => $error ? mb_substr($error, 0, 1000) : null,
                'duration_ms'  => (int) round($durationMs),
                'ran_at'       => now(),
            ]);

            // 清理超出保留数的旧记录
            $keepIds = ScheduledTaskRun::where('command', $key)
                ->orderByDesc('id')
                ->limit(self::KEEP)
                ->pluck('id');
            ScheduledTaskRun::where('command', $key)
                ->whereNotIn('id', $keepIds)
                ->delete();
        } catch (\Throwable) {
            // 日志记录失败不影响任务本身
        }
    }
}
