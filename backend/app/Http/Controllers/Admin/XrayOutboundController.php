<?php

namespace App\Http\Controllers\Admin;

use App\Drivers\Xray\OutboundLinkParser;
use App\Drivers\Xray\WarpService;
use App\Drivers\Xray\XrayRelayService;
use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Models\NodeApiCommand;
use App\Models\XrayInbound;
use App\Models\XrayOutbound;
use App\Services\ThreeXUiClientFactory;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\Cache;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Log;
use Illuminate\Validation\ValidationException;

/**
 * Admin xray 节点出站管理（/admin-api/nodes/{node}/xray-outbounds）。
 *
 * - 结构化 CRUD（协议/服务器/凭据/传输 + 高级 JSON 透传兜底）
 * - 目标来源选「本站服务器」→ 自动中转出站（XrayRelayService::ensureRelay：读对方面板、
 *   在对方建承接凭据、在本地 upsert 中转出站）；路由页只引用已有出站，不再触发中转创建
 * - URI 一键导入（vless/vmess/trojan/ss/socks/http 分享链接）
 * - 连通测试（下发 test_outbound 指令，由节点侧 TCP 探活回执）
 * - 列表附出站流量（agent 上报的 xray_outbound_traffic 覆盖写快照）
 *
 * 任何变更后 bump 该节点 config_version → 节点秒级拉取生效。
 */
class XrayOutboundController extends Controller
{
    use ApiResponse;

    /** 系统保留 tag（与 XrayConfigService 内置出站一致，不可占用）。 */
    private const RESERVED_TAGS = ['direct', 'blocked', 'api'];

    /** 不可作「借道出站」的目标协议：blackhole 无出口、freedom 语义等同直连（与 3x-ui 排除 blackhole 一致）。 */
    private const PROXY_UNSUPPORTED_PROTOCOLS = ['freedom', 'blackhole'];

    /** 3x 目标入站清单（含端口/入站名）缓存时长与键前缀。 */
    private const INBOUND_LIST_TTL = 300;          // 5 分钟
    private const INBOUND_LIST_KEY = 'relay-target-inbounds:';

    public function __construct(
        private readonly XrayRelayService $relay,
        private readonly ThreeXUiClientFactory $threeXFactory,
    ) {}

    /** GET /admin-api/nodes/{node}/xray-outbounds */
    public function index(Node $node): JsonResponse
    {
        $this->ensureXray($node);

        $traffic = DB::table('xray_outbound_traffic')
            ->where('node_id', $node->id)
            ->get()
            ->keyBy('tag');

        $outbounds = $node->xrayOutbounds()->orderBy('sort')->orderBy('id')->get();

        return $this->success([
            'outbounds' => $outbounds->map(fn (XrayOutbound $o) => $this->present($o, $traffic->get($o->tag)))->values()->all(),
            // 目标来源「本站服务器」候选：启用节点 × 启用入站（排除本节点）
            'relay_targets' => $this->relayTargets($node),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** POST /admin-api/nodes/{node}/xray-outbounds */
    public function store(Request $request, Node $node): JsonResponse
    {
        $this->ensureXray($node);

        // 目标来源选「本站服务器」→ 中转自动化（在对方建承接凭据 + 本地建中转出站）
        if ((string) $request->input('target_source') === 'relay') {
            return $this->storeRelay($request, $node);
        }

        // WARP 合并：协议选 WARP → 走 WarpService（注册设备 + 生成 wireguard 出站）。
        // 已有 warp 出站时报错，避免顶掉在用的 WARP 账户/配置。
        if ($request->input('protocol') === 'warp') {
            return $this->storeWarp($node);
        }

        $data = $this->validatePayload($request);

        $outbound = new XrayOutbound();
        $outbound->node_id = $node->id;
        $this->fillFromPayload($outbound, $data, $node, null);
        $outbound->save();

        $node->bumpXrayConfigVersion();

        return $this->success([
            'outbound' => $this->present($outbound->fresh(), null),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** PUT /admin-api/nodes/{node}/xray-outbounds/{outbound} */
    public function update(Request $request, Node $node, XrayOutbound $outbound): JsonResponse
    {
        $this->ensureXray($node);
        $this->ensureOwned($node, $outbound);

        $data = $this->validatePayload($request);
        $this->fillFromPayload($outbound, $data, $node, $outbound);
        $outbound->save();

        $node->bumpXrayConfigVersion();

        return $this->success([
            'outbound' => $this->present($outbound->fresh(), null),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** DELETE /admin-api/nodes/{node}/xray-outbounds/{outbound} */
    public function destroy(Node $node, XrayOutbound $outbound): JsonResponse
    {
        $this->ensureXray($node);
        $this->ensureOwned($node, $outbound);

        if ($outbound->tag === 'warp') {
            throw ValidationException::withMessages(['tag' => 'WARP 出站由 WARP 功能管理，请到 WARP 面板操作']);
        }

        // 中转出站：连带清理对方服务器上的承接凭据（不留垃圾 client）
        if ($outbound->relay_target_node_id !== null) {
            $this->relay->deleteRelayOutbound($outbound);
        } else {
            $outbound->delete();
        }
        $node->bumpXrayConfigVersion();

        return $this->success(['config_version' => $node->xrayConfigVersion()]);
    }

    /**
     * POST /admin-api/nodes/{node}/xray-outbounds/import
     * body: {link: "vless://..."} → 解析为出站结构（不落库，返回预填结构供表单确认后提交）。
     */
    public function import(Request $request, Node $node): JsonResponse
    {
        $this->ensureXray($node);

        $data = $request->validate(['link' => ['required', 'string', 'max:4096']]);

        try {
            $parsed = OutboundLinkParser::parse($data['link']);
        } catch (\InvalidArgumentException $e) {
            return $this->error($e->getMessage());
        }

        return $this->success(['parsed' => $parsed]);
    }

    /**
     * POST /admin-api/nodes/{node}/xray-outbounds/{outbound}/test
     * 下发连通测试指令并同步等待回执（≤8s：WS 通道秒级执行，HTTP 降级 2s 节拍内也够）。
     */
    public function test(Node $node, XrayOutbound $outbound): JsonResponse
    {
        $this->ensureXray($node);
        $this->ensureOwned($node, $outbound);

        [$host, $port] = $this->targetOf($outbound);
        if ($host === '') {
            return $this->error('该出站没有可测试的目标地址');
        }

        $command = NodeApiCommand::create([
            'node_id' => $node->id,
            'user_id' => null,
            'action' => 'test_outbound',
            'payload' => ['host' => $host, 'port' => $port],
            'status' => NodeApiCommand::STATUS_PENDING,
        ]);

        // 同步等待执行回执（节点在线时 2-3 秒内必达；离线超时返回 pending）
        $deadline = microtime(true) + 8;
        while (microtime(true) < $deadline) {
            $fresh = $command->fresh();
            if ($fresh->status === NodeApiCommand::STATUS_SUCCESS || $fresh->status === NodeApiCommand::STATUS_FAILED) {
                return $this->success([
                    'success' => $fresh->status === NodeApiCommand::STATUS_SUCCESS,
                    'output' => (string) $fresh->result,
                ]);
            }
            usleep(500_000);
        }

        return $this->success(['pending' => true, 'message' => '节点未在 8 秒内回执（可能离线），稍后可重试']);
    }

    // ── 内部 ─────────────────────────────────────────────

    /**
     * 「本站服务器」目标来源：调用 XrayRelayService 在对方建承接凭据 + 本地 upsert 中转出站。
     * body: {target_node_id, target_inbound_id, tag?, remark?}
     *   - tag 可选：留空 → 默认「节点名-端口-入站名」；填了则用自定义标识（与已有出站重名报错）
     *   - remark 可选：留空 → 空字符串（不再自动填三段式）；填了则在列表展示
     * → 返回生成的中转出站。
     */
    private function storeRelay(Request $request, Node $node): JsonResponse
    {
        $data = $request->validate([
            'target_node_id' => ['required', 'integer'],
            'target_inbound_id' => ['required', 'integer'],
            'tag' => ['nullable', 'string', 'max:64'],
            'remark' => ['nullable', 'string', 'max:128'],
        ]);

        $tag = $this->relay->ensureRelay(
            $node,
            (int) $data['target_node_id'],
            (int) $data['target_inbound_id'],
            isset($data['tag']) ? (string) $data['tag'] : null,
            isset($data['remark']) ? (string) $data['remark'] : null,
        );

        $outbound = $node->xrayOutbounds()->where('tag', $tag)->firstOrFail();

        $node->bumpXrayConfigVersion();

        return $this->success([
            'outbound' => $this->present($outbound, null),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /**
     * 出站页「目标来源 = 本站服务器」候选：启用服务器 × 启用入站（排除本服务器）。
     * value = {node_id, inbound_id}，label 统一为「服务器名 · 端口 · 入站名 · 协议」。
     *
     * - xray 目标：入站来自 xray_inbounds（inbound_id = xray_inbounds.id），无需打面板
     * - 3x-ui 目标：入站来自 node_inbounds（inbound_id = node_inbounds.inbound_id）。
     *   端口与入站名不在库里，需实时读对方面板 listInbounds 取（5 分钟缓存，key 含节点 id）；
     *   面板不可达时降级为「服务器名 · 协议」，不报错、不阻断开页。
     */
    private function relayTargets(Node $node): array
    {
        $out = [];
        $nodes = Node::query()
            ->where('enabled', true)
            ->where('id', '!=', $node->id)
            ->orderBy('name')
            ->get();

        foreach ($nodes as $target) {
            if ($target->isXray()) {
                foreach (XrayInbound::enabledFor($target) as $in) {
                    $out[] = [
                        'node_id' => $target->id,
                        'node_name' => $target->name,
                        'inbound_id' => $in->id,
                        'port' => (int) $in->port,
                        'protocol' => $in->protocol,
                        'label' => self::relayLabel($target->name, $in->port, $in->tag, $in->protocol),
                        'supported' => $in->protocol !== 'shadowsocks',
                    ];
                }
                continue;
            }

            // 3x-ui 服务器：node_inbounds 只有 inbound_id/protocol，端口/入站名实时读面板（带缓存）
            $meta = $this->threeXInboundMetaMap($target);
            foreach ($target->inbounds as $row) {
                $inboundId = (int) $row->inbound_id;
                $info = $meta[$inboundId] ?? null;

                $out[] = [
                    'node_id' => $target->id,
                    'node_name' => $target->name,
                    'inbound_id' => $inboundId,
                    'port' => $info['port'] ?? null,
                    'protocol' => $row->protocol,
                    'label' => $info !== null
                        ? self::relayLabel($target->name, $info['port'], $info['name'], $row->protocol)
                        : self::relayLabel($target->name, null, null, $row->protocol),
                    'supported' => $row->protocol !== 'shadowsocks',
                ];
            }
        }

        return $out;
    }

    /**
     * 3x-ui 服务器入站清单 → [inbound_id => ['port'=>int, 'name'=>string]]。
     *
     * 端口与入站名（优先备注 remark，无则 tag）都不在 node_inbounds 库里，只能读面板。
     * 结果缓存 5 分钟（key 含节点 id，不含 node 其它属性）；面板不可达时返回空数组，
     * 调用侧据此降级为「服务器名 · 协议」两段文案，不抛不阻断。
     *
     * @return array<int, array{port:int, name:string}>
     */
    private function threeXInboundMetaMap(Node $target): array
    {
        $key = self::INBOUND_LIST_KEY . $target->id;

        try {
            $cached = Cache::get($key);
        } catch (\Throwable) {
            $cached = null; // 缓存不可用（未引导 / driver 异常）→ 当作未命中，直接读面板
        }
        if (is_array($cached)) {
            return $cached;
        }

        try {
            $inbounds = $this->threeXFactory->forNode($target)->listInbounds();
        } catch (\Throwable $e) {
            Log::debug('relay 候选：读取 3x 面板入站清单失败，降级为服务器名·协议', [
                'node_id' => $target->id,
                'error' => $e->getMessage(),
            ]);

            return [];
        }

        $map = [];
        foreach ($inbounds as $inbound) {
            if (! is_array($inbound)) {
                continue;
            }
            $id = (int) ($inbound['id'] ?? 0);
            if ($id === 0) {
                continue;
            }
            $remark = trim((string) ($inbound['remark'] ?? ''));
            $map[$id] = [
                'port' => (int) ($inbound['port'] ?? 0),
                'name' => $remark !== '' ? $remark : (string) ($inbound['tag'] ?? ''),
            ];
        }

        try {
            Cache::put($key, $map, self::INBOUND_LIST_TTL);
        } catch (\Throwable) {
            // 写缓存失败只影响下次是否重读，不影响本次返回
        }

        return $map;
    }

    /** 统一中转候选 label：「服务器名 · 端口 · 入站名 · 协议」，空段自动跳过。 */
    private static function relayLabel(string $node, ?int $port, ?string $name, string $protocol): string
    {
        $parts = array_filter(
            [$node, $port ? (string) $port : '', (string) $name, $protocol],
            fn ($v) => $v !== '',
        );

        return implode(' · ', $parts);
    }

    /**
     * WARP 出站合并入口：注册 WARP 设备（若未注册）并 apply（生成 tag=warp 的 wireguard 出站）。
     * 已有 warp 出站 → 报错（引导去 WARP 面板换 IP / 重置）。
     */
    private function storeWarp(Node $node): JsonResponse
    {
        $existing = $node->xrayOutbounds()->where('tag', 'warp')->exists();
        if ($existing) {
            return $this->error('该节点已有 WARP 出站，请到 WARP 面板管理（换 IP / 重新应用）');
        }

        $service = new WarpService($node);
        try {
            $service->register();
        } catch (\Throwable $e) {
            return $this->error('WARP 注册失败：' . $e->getMessage());
        }

        try {
            $outbound = $service->apply();
        } catch (\Throwable $e) {
            return $this->error('WARP 应用失败：' . $e->getMessage());
        }

        return $this->success([
            'outbound' => $this->present($outbound->fresh(), null),
            'config_version' => $node->fresh()->xrayConfigVersion(),
        ]);
    }

    private function ensureXray(Node $node): void
    {
        if (! $node->isXray()) {
            abort(422, '仅 xray 节点支持该操作');
        }
    }

    private function ensureOwned(Node $node, XrayOutbound $outbound): void
    {
        if ((int) $outbound->node_id !== (int) $node->id) {
            abort(404);
        }
    }

    private function validatePayload(Request $request): array
    {
        return $request->validate([
            // 标识允许中文等任意非空字符（实测 xray 26.3.27 支持中文 tag）；长度/保留字/重名在
            // fillFromPayload 或中转侧另行校验
            'tag' => ['required', 'string', 'max:64', 'regex:/\S/'],
            'protocol' => ['required', 'in:freedom,blackhole,vmess,vless,trojan,shadowsocks,socks,http,wireguard'],
            'remark' => ['nullable', 'string', 'max:128'],
            'enabled' => ['nullable', 'boolean'],
            'sort' => ['nullable', 'integer', 'min:0', 'max:65535'],
            // 通用连接参数（按协议取用）
            'address' => ['nullable', 'string', 'max:255'],
            'port' => ['nullable', 'integer', 'between:1,65535'],
            'user_id' => ['nullable', 'string', 'max:255'], // vless/vmess uuid
            'password' => ['nullable', 'string', 'max:255'], // trojan/ss
            'flow' => ['nullable', 'string', 'max:64'],
            'encryption' => ['nullable', 'string', 'max:64'],
            'method' => ['nullable', 'string', 'max:64'], // ss 加密方式
            'username' => ['nullable', 'string', 'max:255'], // socks/http 用户名
            // 传输
            'network' => ['nullable', 'in:tcp,ws,grpc'],
            'security' => ['nullable', 'in:none,tls'],
            'ws_path' => ['nullable', 'string', 'max:255'],
            'ws_host' => ['nullable', 'string', 'max:255'],
            'tls_server_name' => ['nullable', 'string', 'max:255'],
            // 中转「借道」：本出站的连接先经同节点另一出站拨出（xray sockopt.dialerProxy）
            'dialer_proxy' => ['nullable', 'string', 'max:64'],
            // wireguard（高级透传兜底；WARP 功能外的手工 wireguard）
            'wg_secret_key' => ['nullable', 'string', 'max:255'],
            'wg_address' => ['nullable', 'string', 'max:512'], // 逗号分隔
            'wg_peer_public_key' => ['nullable', 'string', 'max:255'],
            'wg_peer_endpoint' => ['nullable', 'string', 'max:255'],
            'wg_reserved' => ['nullable', 'string', 'max:64'], // "0,0,0"
            // 高级：直接编辑 JSON（非空时优先，UI 未覆盖的协议用）
            'advanced_settings' => ['nullable', 'string', 'max:65535'],
            'advanced_stream' => ['nullable', 'string', 'max:65535'],
        ]);
    }

    private function fillFromPayload(XrayOutbound $outbound, array $data, Node $node, ?XrayOutbound $existing): void
    {
        $protocol = $data['protocol'];
        $tag = (string) $data['tag'];

        if (in_array(strtolower($tag), self::RESERVED_TAGS, true)) {
            throw ValidationException::withMessages(['tag' => "标识 {$tag} 为系统保留"]);
        }
        if (strtolower($tag) === 'warp' && $protocol !== 'wireguard') {
            throw ValidationException::withMessages(['tag' => 'warp 标识预留给 WARP 出站']);
        }

        $dup = $node->xrayOutbounds()->where('tag', $tag)
            ->when($existing, fn ($q) => $q->where('id', '!=', $existing->id))
            ->exists();
        if ($dup) {
            throw ValidationException::withMessages(['tag' => "标识 {$tag} 已存在"]);
        }

        // 高级 JSON 透传（优先）
        $settings = $this->parseAdvanced($data['advanced_settings'] ?? null, 'advanced_settings');
        $stream = $this->parseAdvanced($data['advanced_stream'] ?? null, 'advanced_stream');

        if ($settings === null) {
            $settings = $this->buildSettings($protocol, $data);
        }
        if ($stream === null) {
            $stream = $this->buildStream($protocol, $data, $node, $existing);
        }

        // wireguard 私钥分离存储（加密列）
        if ($protocol === 'wireguard' && ! empty($data['wg_secret_key'])) {
            $outbound->setSecretKey((string) $data['wg_secret_key']);
        }

        $outbound->tag = $tag;
        $outbound->protocol = $protocol;
        $outbound->settings = $settings;
        $outbound->stream_settings = $stream;
        $outbound->remark = (string) ($data['remark'] ?? '') !== '' ? (string) $data['remark'] : null;
        if (array_key_exists('enabled', $data)) {
            $outbound->enabled = (bool) $data['enabled'];
        }
        if (array_key_exists('sort', $data)) {
            $outbound->sort = (int) $data['sort'];
        }
    }

    /** 按协议组装 settings（结构化字段路径）。 */
    private function buildSettings(string $protocol, array $data): array|\stdClass
    {
        $address = (string) ($data['address'] ?? '');
        $port = (int) ($data['port'] ?? 0);

        return match ($protocol) {
            'freedom' => ['domainStrategy' => 'AsIs'],
            'blackhole' => new \stdClass(),
            'vmess' => [
                'vnext' => [[
                    'address' => $this->requireField($address, 'address'),
                    'port' => $this->requirePort($port),
                    'users' => [[
                        'id' => $this->requireField((string) ($data['user_id'] ?? ''), 'user_id'),
                        'alterId' => 0,
                        'security' => (string) ($data['encryption'] ?? 'auto') ?: 'auto',
                    ]],
                ]],
            ],
            'vless' => [
                'vnext' => [[
                    'address' => $this->requireField($address, 'address'),
                    'port' => $this->requirePort($port),
                    'users' => [array_filter([
                        'id' => $this->requireField((string) ($data['user_id'] ?? ''), 'user_id'),
                        'encryption' => 'none',
                        'flow' => (string) ($data['flow'] ?? ''),
                    ], fn ($v) => $v !== '')],
                ]],
            ],
            'trojan' => [
                'servers' => [[
                    'address' => $this->requireField($address, 'address'),
                    'port' => $this->requirePort($port),
                    'password' => $this->requireField((string) ($data['password'] ?? ''), 'password'),
                ]],
            ],
            'shadowsocks' => [
                'servers' => [[
                    'address' => $this->requireField($address, 'address'),
                    'port' => $this->requirePort($port),
                    'method' => $this->requireField((string) ($data['method'] ?? ''), 'method'),
                    'password' => $this->requireField((string) ($data['password'] ?? ''), 'password'),
                ]],
            ],
            'socks', 'http' => [
                'servers' => [[
                    'address' => $this->requireField($address, 'address'),
                    'port' => $this->requirePort($port),
                    'users' => [[
                        'user' => (string) ($data['username'] ?? ''),
                        'pass' => (string) ($data['password'] ?? ''),
                    ]],
                ]],
            ],
            'wireguard' => array_filter([
                // 注：privateKey（secretKey）不落 settings —— 由 secret_key 加密列单独保存，
                // 渲染时统一注入（见 XrayConfigService::outboundSettings）
                'address' => (string) ($data['wg_address'] ?? '') !== ''
                    ? array_map('trim', explode(',', (string) $data['wg_address']))
                    : null,
                'peers' => (string) ($data['wg_peer_public_key'] ?? '') !== '' ? [[
                    'publicKey' => (string) $data['wg_peer_public_key'],
                    'endpoint' => (string) ($data['wg_peer_endpoint'] ?? ''),
                ]] : null,
                'reserved' => (string) ($data['wg_reserved'] ?? '') !== ''
                    ? array_map(fn ($v) => (int) trim($v), explode(',', (string) $data['wg_reserved']))
                    : null,
            ], fn ($v) => $v !== null),
            default => new \stdClass(),
        };
    }

    /**
     * wireguard 特殊处理：settings 里的 secretKey 占位符在生产渲染时由加密列注入。
     * 为了让「高级 JSON 透传」与「表单组装」统一，这里把 secretKey 从 settings 中剥离，
     * 渲染层（XrayConfigService::outboundSettings）统一从 secret_key 列注入。
     */
    /**
     * 按协议组装 streamSettings（结构化字段路径）。
     *
     * 「借道出站」（dialer_proxy）对 freedom/blackhole 不适用，故在早退之前先校验；
     * 校验通过才写入 `sockopt.dialerProxy`。
     */
    private function buildStream(string $protocol, array $data, Node $node, ?XrayOutbound $existing): array
    {
        $dialerProxy = trim((string) ($data['dialer_proxy'] ?? ''));
        if ($dialerProxy !== '') {
            $this->validateDialerProxy($dialerProxy, $node, $existing);
        }

        if (in_array($protocol, ['freedom', 'blackhole'], true)) {
            return [];
        }

        $network = (string) ($data['network'] ?? 'tcp');
        $security = (string) ($data['security'] ?? 'none');

        $stream = ['network' => $network, 'security' => $security];

        if ($network === 'ws') {
            $ws = array_filter([
                'path' => (string) ($data['ws_path'] ?? '') !== '' ? (string) $data['ws_path'] : '/',
                'headers' => (string) ($data['ws_host'] ?? '') !== '' ? ['Host' => (string) $data['ws_host']] : null,
            ], fn ($v) => $v !== null);
            $stream['wsSettings'] = $ws;
        }

        if ($security === 'tls') {
            $stream['tlsSettings'] = array_filter([
                'serverName' => (string) ($data['tls_server_name'] ?? ''),
            ], fn ($v) => $v !== '');
        }

        // 「借道出站」：sockopt.dialerProxy = 同节点另一出站的 tag（排除自身/blackhole/不存在）
        if ($dialerProxy !== '') {
            $stream['sockopt'] = array_merge($stream['sockopt'] ?? [], ['dialerProxy' => $dialerProxy]);
        }

        return $stream;
    }

    /**
     * 校验「借道出站」目标：必须是同节点内已存在、且能作为出口的另一条出站。
     *
     * @param  XrayOutbound|null  $existing  编辑时的自身（自引用直接拒绝）
     */
    private function validateDialerProxy(string $tag, Node $node, ?XrayOutbound $existing): void
    {
        if ($existing !== null && $tag === $existing->tag) {
            throw ValidationException::withMessages(['dialer_proxy' => '借道出站不能指向自身']);
        }

        $target = $node->xrayOutbounds()->where('tag', $tag)
            ->when($existing, fn ($q) => $q->where('id', '!=', $existing->id))
            ->first();

        if ($target === null) {
            throw ValidationException::withMessages(['dialer_proxy' => "借道出站 {$tag} 不存在"]);
        }
        if (in_array($target->protocol, self::PROXY_UNSUPPORTED_PROTOCOLS, true)) {
            throw ValidationException::withMessages(['dialer_proxy' => "借道出站 {$tag}（{$target->protocol}）不能作为出口"]);
        }
    }

    private function requireField(string $value, string $field): string
    {
        if (trim($value) === '') {
            throw ValidationException::withMessages([$field => '该字段必填']);
        }

        return trim($value);
    }

    private function requirePort(int $port): int
    {
        if ($port < 1 || $port > 65535) {
            throw ValidationException::withMessages(['port' => '端口必填（1-65535）']);
        }

        return $port;
    }

    private function parseAdvanced(?string $json, string $fieldName): ?array
    {
        if ($json === null || trim($json) === '') {
            return null;
        }
        $decoded = json_decode($json, true);
        if (! is_array($decoded)) {
            throw ValidationException::withMessages([$fieldName => 'JSON 格式不正确']);
        }

        return $decoded;
    }

    /** 从出站配置提取测试目标（服务器地址 + 端口）。 */
    private function targetOf(XrayOutbound $outbound): array
    {
        $settings = $outbound->settings ?? [];

        foreach (['vnext', 'servers'] as $key) {
            $first = $settings[$key][0] ?? null;
            if (is_array($first) && ! empty($first['address'])) {
                return [(string) $first['address'], (int) ($first['port'] ?? 443)];
            }
        }

        if ($outbound->protocol === 'wireguard') {
            $endpoint = (string) ($settings['peers'][0]['endpoint'] ?? '');
            if ($endpoint !== '' && str_contains($endpoint, ':')) {
                [$h, $p] = explode(':', $endpoint, 2);

                return [$h, (int) $p];
            }
        }

        return ['', 0];
    }

    /** 出站展示结构（不吐凭据明文；流量来自快照表）。 */
    private function present(XrayOutbound $outbound, ?object $traffic): array
    {
        $settings = $outbound->settings ?? [];
        $stream = $outbound->stream_settings ?? [];

        $first = $settings['vnext'][0] ?? $settings['servers'][0] ?? null;

        return [
            'id' => $outbound->id,
            'tag' => $outbound->tag,
            'protocol' => $outbound->protocol,
            'remark' => $outbound->remark,
            'address' => is_array($first) ? ($first['address'] ?? null) : null,
            'port' => is_array($first) ? ($first['port'] ?? null) : null,
            'network' => $stream['network'] ?? null,
            'security' => $stream['security'] ?? null,
            'dialer_proxy' => (string) ($stream['sockopt']['dialerProxy'] ?? '') ?: null,
            // 中转出站：目标服务器（列表展示「去哪台服务器」用；手工出站为 null）
            'relay_target_node_id' => $outbound->relay_target_node_id,
            'relay_target_node_name' => $outbound->relay_target_node_id
                ? Node::query()->whereKey($outbound->relay_target_node_id)->value('name')
                : null,
            'enabled' => $outbound->enabled,
            'sort' => $outbound->sort,
            'settings' => $settings,
            'stream_settings' => $stream,
            'has_secret_key' => $outbound->secret_key !== null,
            'upload' => (int) ($traffic->upload ?? 0),
            'download' => (int) ($traffic->download ?? 0),
            'traffic_updated_at' => isset($traffic->updated_at) ? (string) $traffic->updated_at : null,
        ];
    }
}
