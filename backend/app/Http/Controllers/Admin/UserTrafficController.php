<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\User;
use App\Services\UserTrafficService;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;

/**
 * 用户×机器 流量只读接口（admin-api）。
 *
 * 窗口 24h|7d|30d，非法值回退 24h（同 MonitorController 的口径）。
 * 数据源是同步事务内累加的 user_traffic_hourly / user_traffic_daily，本组不做任何写入。
 */
class UserTrafficController extends Controller
{
    use ApiResponse;

    public function __construct(private UserTrafficService $service)
    {
    }

    /** GET /admin-api/traffic/user-ranking?window=24h|7d|30d&limit=50 */
    public function userRanking(Request $request): JsonResponse
    {
        return $this->success($this->service->userRanking(
            $request->query('window'),
            (int) $request->query('limit', 50),
        ));
    }

    /** GET /admin-api/traffic/node-ranking?window=24h|7d|30d */
    public function nodeRanking(Request $request): JsonResponse
    {
        return $this->success($this->service->nodeRanking($request->query('window')));
    }

    /** GET /admin-api/traffic/users/{user}/nodes?window=24h|7d|30d */
    public function userNodes(Request $request, User $user): JsonResponse
    {
        return $this->success($this->service->userNodeBreakdown((int) $user->id, $request->query('window')));
    }
}
