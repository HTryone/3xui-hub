<?php

namespace App\Traits;

use Illuminate\Http\JsonResponse;

/**
 * 统一 API 响应封装。
 * 成功：{code:0, msg, data}
 * 失败：{code, msg}
 * code === 0 表示成功，非 0 表示错误。
 *
 * 分页列表（纯展示层，见 prompts/pagination-final-2026-10-01）：
 * 成功：{code:0, msg, data:[当前页数组], pagination:{current_page,last_page,total,per_page}}
 * data 仍放当前页的列表数组，分页元信息放顶层 pagination —— 老消费方解 data 数组不受影响。
 */
trait ApiResponse
{
    protected function success(mixed $data = null, string $msg = 'ok', int $httpStatus = 200): JsonResponse
    {
        return response()->json([
            'code' => 0,
            'msg' => $msg,
            'data' => $data,
        ], $httpStatus);
    }

    /**
     * 分页成功响应。每页固定 50 条，不做 per_page 参数。
     *
     * @param \Illuminate\Database\Eloquent\Builder $query 已带稳定排序（orderByDesc('id')）的查询
     * @param callable|null $map 行 → 对外结构的变换；null 则原样输出模型属性
     */
    protected function successPage($query, ?callable $map = null, int $perPage = 50): JsonResponse
    {
        $paginator = $query->paginate($perPage);

        // 页码越界（如 ?page=999）：夹回最后一页而不是报错或给空页，翻页/改 URL 都拿到合理结果
        if ($paginator->currentPage() > $paginator->lastPage()) {
            $paginator = $query->paginate($perPage, ['*'], 'page', $paginator->lastPage());
        }

        $items = collect($paginator->items());
        if ($map !== null) {
            $items = $items->map($map);
        }

        return response()->json([
            'code' => 0,
            'msg' => 'ok',
            'data' => $items->values()->all(),
            'pagination' => [
                'current_page' => $paginator->currentPage(),
                'last_page' => $paginator->lastPage(),
                'total' => $paginator->total(),
                'per_page' => $paginator->perPage(),
            ],
        ], 200);
    }

    protected function error(string $msg, int $code = 400, int $httpStatus = 200): JsonResponse
    {
        return response()->json([
            'code' => $code,
            'msg' => $msg,
            'data' => null,
        ], $httpStatus);
    }
}
