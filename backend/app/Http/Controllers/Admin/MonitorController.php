<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Services\MonitorService;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;

/**
 * 机器流量监控（只读，admin-api）。
 *
 * 数据源是 monitor_samples（L0，5 分钟粒度）：四个查询窗口（24h / 7d / 30d / month）
 * 都落在保留期（默认 90 天）内，所以直接查 L0 聚合即可，不必回读 monitor_daily。
 *
 * window 非法值回退 24h（前端传错参数不该把监控页打挂）。
 */
class MonitorController extends Controller
{
    use ApiResponse;

    public function __construct(private MonitorService $monitor)
    {
    }

    /** GET /admin-api/monitor/summary?window=24h|7d|30d|month */
    public function summary(Request $request): \Illuminate\Http\JsonResponse
    {
        return $this->success($this->monitor->summary($request->query('window')));
    }

    /** GET /admin-api/monitor/series?window=24h|7d|30d|month */
    public function series(Request $request): \Illuminate\Http\JsonResponse
    {
        return $this->success($this->monitor->series($request->query('window')));
    }
}
