<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Services\AsyncTaskService;
use App\Traits\ApiResponse;

/**
 * 流量同步接口（批量优化版）：管理员调用，批量同步所有用户流量。
 * POST /admin-api/sync-traffic
 */
class SyncTrafficController extends Controller
{
    use ApiResponse;

    public function __construct(
        private AsyncTaskService $tasks,
    ) {}

    public function sync(): \Illuminate\Http\JsonResponse
    {
        $nodeIds = Node::where('enabled', true)->pluck('id')->map(fn ($id) => (int) $id)->all();
        $result = $this->tasks->findOrCreateTrafficSync($nodeIds);
        $task = $result['task'];

        return $this->success([
            'task_id' => $task->id,
            'status' => $task->status,
            'queued_nodes' => count($nodeIds),
            'already_running' => !$result['created'],
        ], $result['created'] ? '同步任务已提交' : '已有同步任务正在执行');
    }
}
