<?php

namespace App\Jobs;

use App\Models\Node;
use App\Services\HealthCheckService;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;

/**
 * 节点健康检查 Job（M9.1~M9.2）：遍历 enabled 节点，更新三态 status/latency。
 * 离线节点 status=offline，M6 订阅据此排除（M9.4）。
 */
class HealthCheckJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public function handle(HealthCheckService $service): void
    {
        try {
            // 交给 checkMany 分批并发：api_key 节点同进程并发探测，其余仍逐个同步。
            // 单节点异常已在 checkMany 内部消化，这里的 try/catch 只是最后兜底。
            $service->checkMany(Node::where('enabled', true)->get());
        } catch (\Throwable $e) {
            report($e);
        }
    }
}
