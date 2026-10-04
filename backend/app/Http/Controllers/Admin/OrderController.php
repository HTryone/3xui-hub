<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\Order;
use App\Models\Plan;
use App\Models\User;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;

/**
 * Admin 订单管理。
 * GET /admin-api/orders
 */
class OrderController extends Controller
{
    use ApiResponse;

    /**
     * 列表分页：每页 50 条，orderByDesc('id') 保证翻页稳定。
     *
     * search 在全表里查，不是只过滤当前页（否则对方在第 3 页就等于「搜不到」）。
     * 命中：订单号 / 交易号 / 订单所属用户 ID、邮箱 / 套餐名。
     * 用户和套餐要whereHas 才能按关联字段搜，同时保持 with 预加载不出 N+1。
     */
    public function index(Request $request): \Illuminate\Http\JsonResponse
    {
        $query = Order::with(['user', 'plan', 'paymentConfig'])->orderByDesc('id');

        $search = trim((string) $request->query('search', ''));
        if ($search !== '') {
            $query->where(function ($q) use ($search) {
                $like = '%' . $search . '%';
                $q->where('order_no', 'like', $like)
                    ->orWhere('trade_no', 'like', $like)
                    ->orWhere(function ($sub) use ($search, $like) {
                        $sub->whereHas('user', function ($u) use ($search, $like) {
                            if (ctype_digit($search)) {
                                $u->where('id', (int) $search)->orWhere('email', 'like', $like);
                            } else {
                                $u->where('email', 'like', $like);
                            }
                        });
                    })
                    ->orWhereHas('plan', fn ($p) => $p->where('name', 'like', $like));
            });
        }

        return $this->successPage(
            $query,
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
