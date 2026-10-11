<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\Order;
use App\Traits\ApiResponse;
use Carbon\Carbon;
use Illuminate\Http\JsonResponse;

/**
 * Admin 仪表盘统计。
 * GET /admin-api/stats/revenue → 成交额（今日 / 累计）
 *
 * 只统计 status = paid 的订单，金额取 orders.amount（**实付金额**，已扣优惠码），
 * 不是 original_amount —— 否则优惠码花掉的钱会被算进成交额。
 */
class StatsController extends Controller
{
    use ApiResponse;

    public function revenue(): JsonResponse
    {
        $appTimezone = (string) config('app.timezone');
        $businessTimezone = (string) (config('panel.business_timezone') ?: $appTimezone);

        // 库里的 paid_at 是按 app.timezone 存的（默认 UTC），而「今天」是给人看的日界：
        // 直接用 UTC 日界会把北京时间 0:00–8:00 的订单算到前一天。所以先在业务时区取当天
        // 00:00，再换算成同一个时刻（setTimezone 是换算时刻，不是改字面量）。
        $startOfToday = Carbon::now($businessTimezone)->startOfDay()->setTimezone($appTimezone);

        $paid = fn () => Order::query()->where('status', Order::STATUS_PAID);

        $today = $paid()->where('paid_at', '>=', $startOfToday)
            ->selectRaw('COUNT(*) as orders, COALESCE(SUM(amount), 0) as amount')
            ->first();

        $total = $paid()
            ->selectRaw('COUNT(*) as orders, COALESCE(SUM(amount), 0) as amount')
            ->first();

        return $this->success([
            'timezone' => $businessTimezone,
            'today' => [
                'amount' => round((float) $today->amount, 2),
                'orders' => (int) $today->orders,
            ],
            'total' => [
                'amount' => round((float) $total->amount, 2),
                'orders' => (int) $total->orders,
            ],
        ]);
    }
}
