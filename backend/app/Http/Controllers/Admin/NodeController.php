<?php

namespace App\Http\Controllers\Admin;

use App\Drivers\NodeDriverFactory;
use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Models\NodeInbound;
use App\Models\User;
use App\Services\ThreeXUi\ThreeXUiClient;
use App\Services\UserAdminService;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\DB;

/**
 * Admin 节点管理（M4.3~M4.5）。
 * GET/POST/PUT/DELETE /admin/nodes
 * POST /admin/nodes/{id}/test  → ThreeXUiClient::healthCheck()
 */
class NodeController extends Controller
{
    use ApiResponse;

    public function __construct(
        private NodeDriverFactory $driverFactory,
        private UserAdminService $userService,
    ) {
    }

    public function index(): \Illuminate\Http\JsonResponse
    {
        $nodes = Node::orderByDesc('id')->get();

        return $this->success($nodes->map(fn (Node $n) => $this->present($n))->values());
    }

    public function show(Node $node): \Illuminate\Http\JsonResponse
    {
        return $this->success($this->present($node, true));
    }

    public function store(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $this->validateNode($request);

        $node = DB::transaction(function () use ($data) {
            $node = Node::create([
                'name' => $data['name'],
                'host' => $data['host'],
                'port' => (int) ($data['port'] ?? 443),
                'scheme' => $data['scheme'] ?? 'https',
                'web_base_path' => $data['web_base_path'] ?? '',
                'username' => $data['username'] ?? '',
                'password' => $data['password'] ?? '',
                'api_key' => $data['api_key'] ?? '',
                'enabled' => (bool) ($data['enabled'] ?? true),
                'verify_ssl' => (bool) ($data['verify_ssl'] ?? false),
                'status' => 'offline',
            ]);

            $this->syncInbounds($node, $data['inbounds'] ?? []);

            return $node;
        });

        // ① 接入新节点：先清掉远程 3x-ui 上残留的「本面板托管 client」（email 形如 ch_user_{id}），
        //    使远程从 0 起步，避免重新接入一台曾托管过的机器时旧流量被重复计入。
        //    只删被管 client、保留入站与非托管 client；远程不可达/鉴权失败则静默跳过，不阻塞节点创建。
        $this->cleanupResidualManagedClients($node);

        // 新节点：把所有已有用户同步过去（重建为全新 0 流量 client）
        $this->userService->provisionAllUsersToNode($node);

        return $this->success($this->present($node, true), '创建成功');
    }

    public function update(Request $request, Node $node): \Illuminate\Http\JsonResponse
    {
        $data = $this->validateNode($request, true);

        DB::transaction(function () use ($node, $data) {
            $node->forceFill([
                'name' => $data['name'] ?? $node->name,
                'host' => $data['host'] ?? $node->host,
                'port' => isset($data['port']) ? (int) $data['port'] : $node->port,
                'scheme' => $data['scheme'] ?? $node->scheme,
                'web_base_path' => array_key_exists('web_base_path', $data) ? $data['web_base_path'] : $node->web_base_path,
                'username' => $data['username'] ?? $node->username,
                'password' => array_key_exists('password', $data) ? $data['password'] : $node->password,
                'api_key' => array_key_exists('api_key', $data) ? $data['api_key'] : $node->api_key,
                'enabled' => isset($data['enabled']) ? (bool) $data['enabled'] : $node->enabled,
                'verify_ssl' => isset($data['verify_ssl']) ? (bool) $data['verify_ssl'] : $node->verify_ssl,
            ])->save();

            if (array_key_exists('inbounds', $data)) {
                $this->syncInbounds($node, $data['inbounds'] ?? []);
            }
        });

        return $this->success($this->present($node->fresh(), true), '更新成功');
    }

    public function destroy(Node $node): \Illuminate\Http\JsonResponse
    {
        // ② 销毁节点：先用健康探测判断远程是否可达（请求超时由客户端 timeout 兜底，不会无限挂起）。
        //    可达 → 删除远程上本面板托管的 client；不可达 → 跳过远程操作。
        //    无论远程是否可达，本地节点【必定】删除，根除「删不掉 / 面板卡死」的问题。
        $reachable = false;
        $driver = null;
        try {
            $driver = $this->driverFactory->make($node); // api_key 解密失败会抛，归入不可达
            $health = $driver->healthCheck();
            $reachable = (bool) ($health['ok'] ?? false);
        } catch (\Throwable) {
            $reachable = false;
        }

        if ($reachable && $driver !== null) {
            $inboundIds = $node->inbounds()->pluck('inbound_id')->toArray();
            User::whereNotNull('email')->each(function (User $user) use ($driver, $inboundIds) {
                $email = $user->clientEmail();
                foreach ($inboundIds as $inboundId) {
                    try {
                        $driver->deleteClient($email, false, $inboundId);
                    } catch (\Throwable) {
                        // 不存在，忽略
                    }
                }
                try { $driver->deleteClient($email); } catch (\Throwable) {}
            });
        }

        $node->delete();

        return $this->success(
            null,
            $reachable ? '已删除（远程托管 client 已清理）' : '已删除（远程不可达，已跳过远程清理）'
        );
    }

    /** M4.4 测试连接：healthCheck 并更新节点状态/延迟。 */
    public function test(Node $node): \Illuminate\Http\JsonResponse
    {
        try {
            $driver = $this->driverFactory->make($node);
            $health = $driver->healthCheck();
        } catch (\Throwable $e) {
            $node->forceFill(['status' => 'offline', 'latency' => 0, 'last_check_at' => now()])->save();

            return $this->error('连接失败：' . $e->getMessage(), 500);
        }

        $ok = (bool) $health['ok'];
        $node->forceFill([
            'status' => $ok ? 'online' : 'offline',
            'latency' => (int) ($health['latencyMs'] ?? 0),
            'last_check_at' => now(),
        ])->save();

        if (!$ok) {
            return $this->error('节点离线：' . ($health['error'] ?? 'unknown'), 500);
        }

        return $this->success([
            'ok' => true,
            'latency_ms' => $health['latencyMs'],
            'cpu' => $health['cpu'] ?? null,
            'mem' => $health['mem'] ?? null,
            'xray_state' => $health['xrayState'] ?? null,
        ]);
    }

    /**
     * 拉取入站列表（用于新建/编辑时下拉选择 inbound）。
     * 用当前表单填的连接参数临时连 3x-ui，返回按协议分组的 [{id, label}]。
     * label = 备注 + (协议 :端口)，id 为 3x-ui inbound id。
     */
    public function probeInbounds(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'scheme' => ['sometimes', 'in:http,https'],
            'host' => ['required', 'string'],
            'port' => ['sometimes', 'integer', 'between:1,65535'],
            'web_base_path' => ['sometimes', 'nullable', 'string'],
            'api_key' => ['sometimes', 'nullable', 'string'],
            'username' => ['sometimes', 'nullable', 'string'],
            'password' => ['sometimes', 'nullable', 'string'],
            'verify_ssl' => ['sometimes', 'boolean'],
        ]);

        $client = new ThreeXUiClient([
            'scheme' => $data['scheme'] ?? 'https',
            'host' => $data['host'],
            'port' => $data['port'] ?? 443,
            'web_base_path' => $data['web_base_path'] ?? '',
            'api_key' => $data['api_key'] ?? null,
            'username' => $data['username'] ?? '',
            'password' => $data['password'] ?? '',
            'verify' => (bool) ($data['verify_ssl'] ?? false),
        ]);

        try {
            $inbounds = $client->listInbounds();
        } catch (\Throwable $e) {
            return $this->error('拉取入站失败：' . $e->getMessage(), 500);
        }

        $grouped = ['vless' => [], 'trojan' => []];
        foreach ($inbounds as $in) {
            $proto = $in['protocol'] ?? null;
            if (!array_key_exists($proto, $grouped)) {
                $grouped[$proto] = [];
            }
            $remark = $in['remark'] ?? $in['tag'] ?? ('inbound-' . $in['id']);
            $port = $in['port'] ?? '?';
            $grouped[$proto][] = [
                'id' => (int) $in['id'],
                'remark' => $remark,
                'label' => $remark . ' (' . $proto . ' :' . $port . ')',
            ];
        }

        return $this->success($grouped);
    }

    private function validateNode(Request $request, bool $forUpdate = false): array
    {
        $rules = [
            'name' => [$forUpdate ? 'sometimes' : 'required', 'string', 'max:120'],
            'host' => [$forUpdate ? 'sometimes' : 'required', 'string', 'max:200'],
            'port' => ['sometimes', 'integer', 'between:1,65535'],
            'scheme' => ['sometimes', 'in:http,https'],
            'web_base_path' => ['sometimes', 'nullable', 'string', 'max:200'],
            'username' => ['sometimes', 'nullable', 'string'],
            'password' => ['sometimes', 'nullable', 'string'],
            'api_key' => ['sometimes', 'nullable', 'string'],
            'enabled' => ['sometimes', 'boolean'],
            'verify_ssl' => ['sometimes', 'boolean'],
            'inbounds' => ['sometimes', 'array'],
            'inbounds.vless' => ['sometimes', 'nullable', 'array'],
            'inbounds.vless.*' => ['integer'],
            'inbounds.trojan' => ['sometimes', 'nullable', 'array'],
            'inbounds.trojan.*' => ['integer'],
        ];

        return $request->validate($rules);
    }

    private function syncInbounds(Node $node, array $inbounds): void
    {
        $oldInbounds = $node->inbounds()->get()->keyBy('protocol');

        $node->inbounds()->delete();

        foreach (['vless', 'trojan'] as $proto) {
            $ids = $inbounds[$proto] ?? [];
            // 兼容单值（旧数据/前端过渡）
            if (!is_array($ids)) {
                $ids = $ids !== null ? [(int) $ids] : [];
            }
            foreach ($ids as $id) {
                NodeInbound::create([
                    'node_id' => $node->id,
                    'protocol' => $proto,
                    'inbound_id' => (int) $id,
                ]);
            }

            // 入站变化时，把用户同步到新入站
            $oldIds = $oldInbounds->has($proto) ? [$oldInbounds[$proto]->inbound_id] : [];
            sort($oldIds);
            $newIds = array_map('intval', $ids);
            sort($newIds);
            if ($oldIds !== $newIds && !empty($newIds)) {
                $this->syncUsersToInbounds($node, $proto, $newIds);
            }
        }
    }

    /**
     * 把指定协议的所有用户 attach 到新入站列表。
     */
    private function syncUsersToInbounds(Node $node, string $proto, array $inboundIds): void
    {
        $users = \App\Models\User::where('protocol', $proto)->whereNotNull('plan_id')->get();
        $driver = $this->driverFactory->make($node);

        foreach ($users as $user) {
            try {
                $existing = $driver->getClient($user->clientEmail());
                if ($existing) {
                    $driver->attachClient($user->clientEmail(), $inboundIds);
                }
            } catch (\Throwable $e) {
                report($e);
            }
        }
    }

    /**
     * 接入新节点时，删除远程 3x-ui 上残留的「本面板托管 client」（email 形如 ch_user_{id}）。
     * 目的：让远程从 0 起步，避免重新接入一台曾托管过的机器时旧流量被重复计入。
     * 只删被管 client、保留入站与非托管 client；远程不可达/出错时静默跳过，不阻塞节点创建。
     */
    private function cleanupResidualManagedClients(Node $node): void
    {
        try {
            $driver = $this->driverFactory->make($node);
            $clients = $driver->listClients();
        } catch (\Throwable $e) {
            report($e);

            return; // 远程不可达或鉴权失败，跳过清理（后续 provision 按现状处理）
        }

        foreach ($clients as $client) {
            $email = $client['email'] ?? '';
            if ($email === '' || !preg_match('/^ch_user_\d+$/', $email)) {
                continue;
            }
            try {
                $driver->deleteClient($email); // keepTraffic=false → 删除并重置流量
            } catch (\Throwable $e) {
                report($e);
            }
        }
    }

    private function present(Node $n, bool $full = false): array
    {
        $data = [
            'id' => $n->id,
            'name' => $n->name,
            'host' => $n->host,
            'port' => $n->port,
            'scheme' => $n->scheme,
            'web_base_path' => $n->web_base_path,
            'username' => $n->username,
            'enabled' => (bool) $n->enabled,
            'verify_ssl' => (bool) $n->verify_ssl,
            'status' => $n->status,
            'latency' => $n->latency,
            'last_check_at' => $n->last_check_at?->toIso8601String(),
            'inbounds' => $n->inbounds->groupBy('protocol')->map(fn ($items) => $items->pluck('inbound_id')->values())->all(),
        ];

        if ($full) {
            // api_key 解密回填到编辑表单（admin 本人有权限看自己管理的节点 token）
            $data['api_key'] = $n->api_key;
            $data['has_api_key'] = $n->api_key !== null;
            // 面板登录密码较敏感，仅返回布尔，编辑留空即不改
            $data['has_password'] = $n->password !== null;
        }

        return $data;
    }
}
