<?php

namespace App\Http\Controllers\Admin;

use App\Drivers\NodeDriverFactory;
use App\Http\Controllers\Controller;
use App\Models\AsyncTask;
use App\Models\Node;
use App\Models\NodeInbound;
use App\Services\NodeCleanupService;
use App\Services\NodeInboundSyncService;
use App\Services\NodeInitService;
use App\Services\ThreeXUi\ThreeXUiClient;
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
        private NodeInitService $nodeInit,
        private NodeInboundSyncService $inboundSync,
        private NodeCleanupService $nodeCleanup,
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
        $shouldEnable = (bool) ($data['enabled'] ?? true);

        $selectedInboundIds = $this->selectedInboundIds($data['inbounds'] ?? []);
        if ($selectedInboundIds === []) {
            return $this->error('请至少选择一个入站');
        }

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
                'enabled' => false,
                'verify_ssl' => (bool) ($data['verify_ssl'] ?? false),
                'status' => 'offline',
            ]);

            $this->syncInbounds($node, $data['inbounds'] ?? [], false);

            return $node;
        });

        // P1-1/P1-2 修复：初始化异步化——按用户逐条派发，单用户失败不影响其余；
        // 全部成功（且勾选启用）才由 maybeFinalize 启用节点；有失败只标 offline，不改 enabled。
        $task = $this->nodeInit->submit($node, $shouldEnable);
        $message = '创建成功，接入初始化进行中（日志页可跟踪进度）';

        return $this->success(
            $this->present($node->fresh(), true) + [
                'init_task_id' => $task->id,
                'init_status' => $task->status,
                'message' => $message,
            ],
            $message
        );
    }

    public function update(Request $request, Node $node): \Illuminate\Http\JsonResponse
    {
        $data = $this->validateNode($request, true);

        if (array_key_exists('inbounds', $data) && $this->selectedInboundIds($data['inbounds'] ?? []) === []) {
            return $this->error('请至少选择一个入站');
        }

        $activeInitTask = AsyncTask::query()
            ->where('type', 'node_init')
            ->where('subject_type', 'node')
            ->where('subject_id', $node->id)
            ->whereIn('status', [AsyncTask::STATUS_PENDING, AsyncTask::STATUS_RUNNING])
            ->latest('id')
            ->first();
        if ($activeInitTask) {
            if ($this->changesInitConfiguration($node, $data)) {
                return $this->error('节点初始化进行中，暂不能修改连接信息或入站');
            }
            if (array_key_exists('enabled', $data)) {
                $updated = $this->nodeInit->updateDesiredEnabled($node->id, $activeInitTask->id, (bool) $data['enabled']);
                if ($updated) {
                    unset($data['enabled']);
                }
            }
        }

        $syncTask = null;
        DB::transaction(function () use ($node, $data, &$syncTask) {
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
                $syncTask = $this->syncInbounds($node, $data['inbounds'] ?? []);
            }
        });

        $payload = $this->present($node->fresh(), true);

        // 入站变了且真有用户要搬：请求内一个面板请求都不发，只回报任务 id 让前端提示
        if ($syncTask !== null) {
            $message = '已更新，用户正在后台同步到新入站（日志页可看进度）';

            return $this->success($payload + ['sync_task_id' => $syncTask->id, 'message' => $message], $message);
        }

        return $this->success($payload, '更新成功');
    }

    /**
     * 删除节点：本地行立刻删，远端 client 清理交给后台任务。
     *
     * 旧实现在请求里先 healthCheck 再逐用户 deleteClient（100 用户 = 100+ 次串行 HTTPS）。
     * 现在清理任务自带可达性判断，不可达/部分失败都会写进任务结果（日志页可见、可重试）。
     * 连接快照必须在 $node->delete() 之前取。
     */
    public function destroy(Node $node): \Illuminate\Http\JsonResponse
    {
        $task = $this->nodeCleanup->submit($node);
        $node->delete();

        $message = '已删除，正在后台清理远端客户端（日志页可看进度）';

        return $this->success([
            'cleanup_task_id' => $task->id,
            'message' => $message,
        ], $message);
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

    private function selectedInboundIds(array $inbounds): array
    {
        return array_merge(
            is_array($inbounds['vless'] ?? null) ? $inbounds['vless'] : [],
            is_array($inbounds['trojan'] ?? null) ? $inbounds['trojan'] : [],
        );
    }

    private function changesInitConfiguration(Node $node, array $data): bool
    {
        foreach (['host', 'port', 'scheme', 'web_base_path', 'username', 'password', 'api_key', 'verify_ssl'] as $field) {
            if (array_key_exists($field, $data) && $data[$field] != $node->{$field}) {
                return true;
            }
        }

        if (!array_key_exists('inbounds', $data)) {
            return false;
        }

        foreach (['vless', 'trojan'] as $protocol) {
            $current = $node->inboundIdsFor($protocol);
            $incoming = array_map('intval', $data['inbounds'][$protocol] ?? []);
            sort($current);
            sort($incoming);
            if ($current !== $incoming) {
                return true;
            }
        }

        return false;
    }

    /**
     * 落库节点入站；协议入站集合发生变化时，把用户搬迁工作派发成后台任务。
     *
     * 与旧实现的差异只在「谁来发面板请求」：旧的在请求里逐用户 getClient + attachClient，
     * 现在只算差异 + 派发（update() 请求内零面板请求）。
     *
     * @param bool $syncUsers false 表示只落库不搬用户（新建节点走初始化流程，见 store()）
     * @return AsyncTask|null 派发出的用户同步任务；无需搬用户时为 null
     */
    private function syncInbounds(Node $node, array $inbounds, bool $syncUsers = true): ?AsyncTask
    {
        $oldInbounds = $node->inbounds()
            ->get()
            ->groupBy('protocol')
            ->map(fn ($items) => $items->pluck('inbound_id')->map(fn ($id) => (int) $id)->all());

        $node->inbounds()->delete();

        $changed = [];
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

            // 入站变化时，把用户同步到新入站（判定条件与拆分前逐一保持）
            $oldIds = $oldInbounds->get($proto, []);
            sort($oldIds);
            $newIds = array_map('intval', $ids);
            sort($newIds);
            if ($syncUsers && $oldIds !== $newIds && !empty($newIds)) {
                $changed[$proto] = $newIds;
            }
        }

        return $changed === [] ? null : $this->inboundSync->submit($node, $changed);
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
