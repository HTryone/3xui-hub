<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Models\SiteConfig;
use App\Services\TrafficSyncService;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;

/**
 * 用户端节点（M4.6）：GET /api/nodes
 * 返回当前用户可用协议下、enabled 且 status=online 的节点。
 */
class NodeController extends Controller
{
    use ApiResponse;

    public function index(Request $request): \Illuminate\Http\JsonResponse
    {
        $user = $request->user();

        $nodes = Node::where('enabled', true)
            ->where('status', 'online')
            ->whereHas('inbounds', function ($q) use ($user) {
                $q->where('protocol', $user->protocol);
            })
            ->get();

        // site_configs.show_node_multiplier：默认关（没有这行 = 关）
        $showMultiplier = in_array(SiteConfig::getValue('show_node_multiplier', ''), ['1', 'true', 'on'], true);

        return $this->success($nodes->map(function (Node $n) use ($showMultiplier) {
            $item = [
                'id' => $n->id,
                'name' => $n->name,
                'host' => $n->host,
                'port' => $n->port,
                'latency' => $n->latency,
                'status' => $n->status,
            ];

            // 开关关着（默认）时整个字段都不出现，行为与加倍率之前完全一致。
            // 开关开着时继承和手动设的都返回实际生效值 —— 手动设的更要让人知道。
            // 展示值与计费值必须同源：统一走 TrafficSyncService::multiplierFor()，
            // 不允许在这里再写一套取值（否则展示与计费将来必分叉）。
            if ($showMultiplier) {
                $item['traffic_multiplier'] = TrafficSyncService::multiplierFor($n);
                $item['traffic_multiplier_source'] = $n->traffic_multiplier !== null ? 'node' : 'default';
            }

            return $item;
        })->values());
    }
}
