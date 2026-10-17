<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Services\GeodataService;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;

/**
 * Geo 数据管理（geoip.dat / geosite.dat 自动更新与分发，issue #40）。
 *
 * GET  /admin-api/geodata          面板文件状态 + 各节点最近同步状态
 * POST /admin-api/geodata/update   立即从上游检查并更新（同步执行；前端单独放宽超时）
 * POST /admin-api/geodata/dispatch 把面板当前文件分发到所有 enabled xray 节点
 */
class GeodataController extends Controller
{
    use ApiResponse;

    public function __construct(private readonly GeodataService $geo) {}

    public function index(): JsonResponse
    {
        return $this->success([
            'files' => $this->geo->status(),
            'nodes' => $this->geo->nodeStatuses(),
        ]);
    }

    public function update(): JsonResponse
    {
        $results = $this->geo->update();

        $failed = array_filter($results, fn ($r) => $r['status'] === 'failed');
        $updated = array_filter($results, fn ($r) => $r['status'] === 'updated');

        $msg = match (true) {
            $failed !== [] => '部分文件更新失败，详见结果',
            $updated !== [] => '已更新 ' . count($updated) . ' 个文件，可分发到节点',
            default => '已是最新，无需更新',
        };

        return $this->success(['results' => $results], $msg);
    }

    public function dispatch(): JsonResponse
    {
        $res = $this->geo->dispatchToNodes();

        if ($res['missing']) {
            return $this->error('面板上还没有 geo 文件，请先执行「检查并更新」');
        }

        $msg = "已下发 {$res['queued']} 个节点" . ($res['skipped'] > 0 ? "（{$res['skipped']} 个节点已有在途任务，跳过）" : '');

        return $this->success($res, $msg);
    }
}
