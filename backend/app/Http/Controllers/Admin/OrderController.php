<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\Order;
use App\Traits\ApiResponse;

/**
 * Admin 订单管理。
 * GET /admin-api/orders
 */
class OrderController extends Controller
{
    use ApiResponse;

    /** 列表分页：每页 50 条，orderByDesc('id') 保证翻页稳定。 */
    public function index(): \Illuminate\Http\JsonResponse
    {
        return $this->successPage(
            Order::with(['user', 'plan', 'paymentConfig'])->orderByDesc('id'),
            fn (Order $o) => [
                'order_no' => $o->order_no,
                'user_id' => $o->user_id,
                'user_email' => $o->user?->email,
                'plan_name' => $o->plan?->name ?? '-',
                'amount' => (float) $o->amount,
                'status' => $o->status,
                'payment_name' => $o->paymentConfig?->name,
                'trade_no' => $o->trade_no,
                'paid_at' => $o->paid_at?->toIso8601String(),
                'created_at' => $o->created_at->toIso8601String(),
            ],
        );
    }
}
