<?php

namespace App\Http\Controllers\Admin;

use App\Drivers\Xray\RealityKeyService;
use App\Drivers\Xray\RealityTargetCatalog;
use App\Drivers\Xray\XrayConfigService;
use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Models\NodeApiCommand;
use App\Models\User;
use App\Models\XrayInbound;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Collection;
use Illuminate\Validation\ValidationException;

/**
 * Admin xray 节点入站管理（/admin-api/nodes/{node}/xray-inbounds）。
 *
 * 入站协议面板自建：vless / vmess / trojan / shadowsocks
 * （含 tcp/ws 传输、none/tls/reality 安全层；reality 仅 vless）。
 * Reality 密钥面板生成（X25519，与 xray x25519 对拍兼容）。
 *
 * 任何变更后 bump 该节点 config_version → WS 服务推送 config.changed → 节点秒级拉取。
 */
class XrayInboundController extends Controller
{
    use ApiResponse;

    public function __construct(
        private readonly XrayConfigService $config,
    ) {}

    /** GET /admin-api/nodes/{node}/xray-inbounds */
    public function index(Node $node): JsonResponse
    {
        $this->ensureXray($node);

        $inbounds = $node->xrayInbounds()->orderBy('sort')->orderBy('id')->get();
        $users = $this->config->eligibleUsers();

        return $this->success([
            'inbounds' => $inbounds->map(fn (XrayInbound $in) => $this->present($in, $users))->values()->all(),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** POST /admin-api/nodes/{node}/xray-inbounds */
    public function store(Request $request, Node $node): JsonResponse
    {
        $this->ensureXray($node);
        $data = $this->validatePayload($request);
        $this->guardServerNames($request, $data);

        $inbound = new XrayInbound();
        $inbound->node_id = $node->id;
        $this->fillFromPayload($inbound, $data, $node, null);
        $inbound->save();

        $node->bumpXrayConfigVersion();

        return $this->success([
            'inbound' => $this->present($inbound->fresh(), $this->config->eligibleUsers()),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** PUT /admin-api/nodes/{node}/xray-inbounds/{inbound} */
    public function update(Request $request, Node $node, XrayInbound $inbound): JsonResponse
    {
        $this->ensureXray($node);
        $this->ensureOwned($node, $inbound);

        $data = $this->validatePayload($request, $inbound);
        $this->guardServerNames($request, $data, $inbound);
        $this->fillFromPayload($inbound, $data, $node, $inbound);
        $inbound->save();

        $node->bumpXrayConfigVersion();

        return $this->success([
            'inbound' => $this->present($inbound->fresh(), $this->config->eligibleUsers()),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** DELETE /admin-api/nodes/{node}/xray-inbounds/{inbound} */
    public function destroy(Node $node, XrayInbound $inbound): JsonResponse
    {
        $this->ensureXray($node);
        $this->ensureOwned($node, $inbound);

        $inbound->delete();
        $node->bumpXrayConfigVersion();

        return $this->success(['config_version' => $node->xrayConfigVersion()]);
    }

    /**
     * POST /admin-api/nodes/{node}/xray-inbounds/batch-delete
     * body: { ids: number[] } —— 一次删多个入站（对齐 3x-ui 入站列表的多选删除）。
     *
     * 口径：
     * - 只认本节点下的 id，不在本节点的 id 归入 failed（reason=not_found）原样返回（不报错、不静默吞）
     * - 逐个删；单条失败不影响其余（与 3x-ui「逐条处理、按 id 回报」一致）
     * - 全部处理完只 bump 一次配置版本，节点只重载一次配置
     */
    public function batchDestroy(Request $request, Node $node): JsonResponse
    {
        $this->ensureXray($node);

        $data = $request->validate([
            'ids' => ['required', 'array', 'min:1', 'max:100'],
            'ids.*' => ['integer', 'min:1'],
        ]);

        $ids = array_values(array_unique(array_map('intval', $data['ids'])));
        $targets = $node->xrayInbounds()->whereIn('id', $ids)->get()->keyBy('id');

        $deleted = [];
        $failed = [];

        foreach ($ids as $id) {
            $inbound = $targets->get($id);
            if (! $inbound) {
                $failed[] = ['id' => $id, 'reason' => 'not_found'];
                continue;
            }
            try {
                $inbound->delete();
                $deleted[] = $id;
            } catch (\Throwable $e) {
                $failed[] = ['id' => $id, 'reason' => $e->getMessage()];
            }
        }

        if ($deleted) {
            $node->bumpXrayConfigVersion();
        }

        return $this->success([
            'deleted' => $deleted,
            'failed' => $failed,
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /**
     * POST /admin-api/nodes/{node}/xray-inbounds/reality-keypair
     * 生成一组 Reality 密钥材料（新建/轮换表单预填；与 xray x25519 格式一致）。
     */
    public function realityKeypair(Node $node): JsonResponse
    {
        $this->ensureXray($node);

        [$privateKey, $publicKey] = RealityKeyService::generateKeypair();

        return $this->success([
            'private_key' => $privateKey,
            'public_key' => $publicKey,
            'short_id' => RealityKeyService::shortId(),
            'dest' => XrayConfigService::DEFAULT_DEST,
            'server_name' => XrayConfigService::DEFAULT_SNI,
        ]);
    }

    /**
     * POST /admin-api/nodes/{node}/xray-inbounds/reality-scan
     * body: {targets?: string[]} —— 留空用面板内置候选（RealityTargetCatalog::defaults）。
     *
     * 探测在**节点**侧跑：REALITY 的伪装回源是节点去连 dest，只有节点视角的延迟才有意义
     * （面板视角在国内/跨区场景会完全失真）。复用 node_api_commands 指令通道，
     * 节点在线时秒级回执，离线/超时返回 pending。
     */
    public function realityScan(Request $request, Node $node): JsonResponse
    {
        $this->ensureXray($node);

        $data = $request->validate([
            'targets' => ['nullable', 'array', 'max:20'],
            // nullable：空串会被 ConvertEmptyStringsToNull 变成 null（前端已过滤，这里兜底）
            'targets.*' => ['nullable', 'string', 'max:255'],
        ]);

        $targets = RealityTargetCatalog::sanitize($data['targets'] ?? []);
        if ($targets === []) {
            $targets = RealityTargetCatalog::defaults();
        }

        $command = NodeApiCommand::create([
            'node_id' => $node->id,
            'user_id' => null,
            'action' => NodeApiCommand::ACTION_REALITY_SCAN,
            'payload' => ['targets' => $targets],
            'status' => NodeApiCommand::STATUS_PENDING,
        ]);

        // 同步等待回执（默认 ≤18s：节点侧并发探测，正常 5s 内出结果；离线节点返回 pending）
        $wait = max(0.0, (float) config('panel.reality_scan_wait', 18));
        $deadline = microtime(true) + $wait;
        while (microtime(true) < $deadline) {
            $fresh = $command->fresh();
            if ($fresh->status === NodeApiCommand::STATUS_FAILED) {
                $output = (string) $fresh->result;

                return $this->success([
                    'results' => [],
                    // 旧版 agent 不认这个 action，直说怎么修，别把 "unknown action" 甩给管理员
                    'error' => str_contains($output, 'unknown action')
                        ? '该节点的 agent 版本过旧，请先在「节点」页升级 agent 后重试'
                        : ($output !== '' ? $output : '节点执行失败'),
                ]);
            }
            if ($fresh->status === NodeApiCommand::STATUS_SUCCESS) {
                $results = json_decode((string) $fresh->result, true);
                if (! is_array($results)) {
                    return $this->success(['results' => [], 'error' => '节点回执解析失败']);
                }

                return $this->success(['results' => $results]);
            }
            usleep(300_000);
        }

        return $this->success([
            'pending' => true,
            'message' => sprintf('节点未在 %d 秒内回执（可能离线），稍后可重试', (int) ceil($wait)),
        ]);
    }

    // ── 内部 ─────────────────────────────────────────────

    private function ensureXray(Node $node): void
    {
        if (! $node->isXray()) {
            abort(422, '仅 xray 节点支持该操作');
        }
    }

    private function ensureOwned(Node $node, XrayInbound $inbound): void
    {
        if ((int) $inbound->node_id !== (int) $node->id) {
            abort(404);
        }
    }

    private function validatePayload(Request $request, ?XrayInbound $existing = null): array
    {
        return $request->validate([
            'protocol' => ['required', 'in:vless,vmess,trojan,shadowsocks'],
            'tag' => ['nullable', 'string', 'max:64', 'regex:/^[A-Za-z0-9_-]+$/'],
            // 备注：tag 是引用型标识（路由/删用户/中转拼接），中文名只能放这里；
            // 订阅节点名展示用，空则回落 tag
            'remark' => ['nullable', 'string', 'max:128'],
            'port' => ['required', 'integer', 'between:1,65535'],
            'listen' => ['nullable', 'string', 'max:64'],
            'network' => ['nullable', 'in:tcp,ws,grpc,xhttp,httpupgrade'],
            'security' => ['nullable', 'in:none,tls,reality'],
            // 嗅探（issue #39）：不提交时行为不变；提交时结构受控
            'sniffing' => ['nullable', 'array'],
            'sniffing.enabled' => ['nullable', 'boolean'],
            'sniffing.destOverride' => ['nullable', 'array', 'max:4'],
            'sniffing.destOverride.*' => ['string', 'in:http,tls,quic,fakedns'],
            'sniffing.routeOnly' => ['nullable', 'boolean'],
            // reality 参数
            'dest' => ['nullable', 'string', 'max:255'],
            'server_names' => ['nullable', 'array', 'max:64'],
            'server_names.*' => ['string', 'max:255'],
            'short_ids' => ['nullable', 'array', 'max:8'],
            'short_ids.*' => ['string', 'max:16', 'regex:/^[0-9a-fA-F]+$/'],
            'private_key' => ['nullable', 'string', 'max:255'],
            'public_key' => ['nullable', 'string', 'max:255'],
            // reality 高级参数（3x-ui reality.ts 字段集；服务端字段 + 客户端字段）
            'show' => ['nullable', 'boolean'],
            'xver' => ['nullable', 'integer', 'min:0', 'max:2'],
            'min_client_ver' => ['nullable', 'string', 'max:32', 'regex:/^\d+(\.\d+)*$/'],
            'max_client_ver' => ['nullable', 'string', 'max:32', 'regex:/^\d+(\.\d+)*$/'],
            'max_timediff' => ['nullable', 'integer', 'min:0', 'max:86400000'],
            'mldsa65_seed' => ['nullable', 'string', 'max:8192'],
            'mldsa65_verify' => ['nullable', 'string', 'max:8192'],
            'master_key_log' => ['nullable', 'string', 'max:255'],
            'fingerprint' => ['nullable', 'string', 'max:32'],
            'spider_x' => ['nullable', 'string', 'max:255'],
            'limit_fallback_upload' => ['nullable', 'array'],
            'limit_fallback_upload.after_bytes' => ['nullable', 'integer', 'min:0'],
            'limit_fallback_upload.bytes_per_sec' => ['nullable', 'integer', 'min:0'],
            'limit_fallback_upload.burst_bytes_per_sec' => ['nullable', 'integer', 'min:0'],
            'limit_fallback_download' => ['nullable', 'array'],
            'limit_fallback_download.after_bytes' => ['nullable', 'integer', 'min:0'],
            'limit_fallback_download.bytes_per_sec' => ['nullable', 'integer', 'min:0'],
            'limit_fallback_download.burst_bytes_per_sec' => ['nullable', 'integer', 'min:0'],
            // ws / httpupgrade / tcp 伪装 / tls 参数
            'ws_path' => ['nullable', 'string', 'max:255'],
            'ws_host' => ['nullable', 'string', 'max:255'],
            'httpupgrade_path' => ['nullable', 'string', 'max:255'],
            'httpupgrade_host' => ['nullable', 'string', 'max:255'],
            // tcp header 伪装（type=http 时启用 http 伪装）
            'tcp_header_type' => ['nullable', 'in:none,http'],
            'tcp_header_path' => ['nullable', 'string', 'max:255'],
            'tcp_header_host' => ['nullable', 'string', 'max:255'],
            'xhttp_path' => ['nullable', 'string', 'max:255'],
            'xhttp_host' => ['nullable', 'string', 'max:255'],
            'xhttp_mode' => ['nullable', 'string', 'max:32'],
            // xhttp 高级字段（xray-core SplitHTTPConfig = 3x-ui xhttp.ts 字段集；
            // 空值不落库 —— 未配置即内核默认，避免把默认值写进配置成为 DPI 指纹）
            'xhttp_padding_bytes' => ['nullable', 'string', 'max:64'],
            'xhttp_padding_obfs_mode' => ['nullable', 'boolean'],
            'xhttp_padding_key' => ['nullable', 'string', 'max:128'],
            'xhttp_padding_header' => ['nullable', 'string', 'max:128'],
            'xhttp_padding_placement' => ['nullable', 'string', 'max:32'],
            'xhttp_padding_method' => ['nullable', 'string', 'max:32'],
            'xhttp_session_id_placement' => ['nullable', 'string', 'max:32'],
            'xhttp_session_id_key' => ['nullable', 'string', 'max:128'],
            'xhttp_session_id_table' => ['nullable', 'string', 'max:64'],
            'xhttp_session_id_length' => ['nullable', 'string', 'max:32'],
            'xhttp_seq_placement' => ['nullable', 'string', 'max:32'],
            'xhttp_seq_key' => ['nullable', 'string', 'max:128'],
            'xhttp_uplink_data_placement' => ['nullable', 'string', 'max:32'],
            'xhttp_uplink_data_key' => ['nullable', 'string', 'max:128'],
            'xhttp_uplink_chunk_size' => ['nullable', 'integer', 'min:0', 'max:1048576'],
            'xhttp_sc_max_each_post_bytes' => ['nullable', 'string', 'max:64'],
            'xhttp_sc_max_buffered_posts' => ['nullable', 'integer', 'min:0', 'max:100000'],
            'xhttp_sc_min_posts_interval_ms' => ['nullable', 'string', 'max:64'],
            'xhttp_sc_stream_up_server_secs' => ['nullable', 'string', 'max:64'],
            'xhttp_server_max_header_bytes' => ['nullable', 'integer', 'min:0', 'max:1048576'],
            'xhttp_uplink_http_method' => ['nullable', 'string', 'max:16'],
            'xhttp_no_sse_header' => ['nullable', 'boolean'],
            'xhttp_no_grpc_header' => ['nullable', 'boolean'],
            'xhttp_headers' => ['nullable', 'string', 'max:8192'],
            'xhttp_xmux_max_concurrency' => ['nullable', 'string', 'max:32'],
            'xhttp_xmux_max_connections' => ['nullable', 'string', 'max:32'],
            'xhttp_xmux_c_max_reuse_times' => ['nullable', 'string', 'max:32'],
            'xhttp_xmux_h_max_request_times' => ['nullable', 'string', 'max:32'],
            'xhttp_xmux_h_max_reusable_secs' => ['nullable', 'string', 'max:32'],
            'xhttp_xmux_h_keep_alive_period' => ['nullable', 'string', 'max:32'],
            'tls_server_name' => ['nullable', 'string', 'max:255'],
            'tls_certificate' => ['nullable', 'string', 'max:65535'],
            'tls_key' => ['nullable', 'string', 'max:65535'],
            // shadowsocks
            'ss_method' => ['nullable', 'in:aes-256-gcm,aes-128-gcm,chacha20-poly1305,xchacha20-poly1305,2022-blake3-aes-256-gcm,2022-blake3-aes-128-gcm'],
            'ss_password' => ['nullable', 'string', 'max:128'],
            // 开关
            'enabled' => ['nullable', 'boolean'],
            'sort' => ['nullable', 'integer', 'min:0', 'max:65535'],
        ]);
    }

    /** 表单载荷 → 入站模型（settings / stream_settings / 密钥）。 */
    private function fillFromPayload(XrayInbound $inbound, array $data, Node $node, ?XrayInbound $existing): void
    {
        $protocol = $data['protocol'];
        $network = $data['network'] ?? 'tcp';
        $security = $data['security'] ?? 'none';

        // 组合合法性（前后端同守）
        if ($security === 'reality' && $protocol !== 'vless') {
            throw ValidationException::withMessages(['security' => 'Reality 仅支持 VLESS 协议']);
        }
        // Reality 传输面（xray-core 实测：REALITY only supports RAW, XHTTP and gRPC）
        if ($security === 'reality' && ! in_array($network, ['tcp', 'xhttp', 'grpc'], true)) {
            throw ValidationException::withMessages(['network' => 'Reality 仅支持 tcp / xhttp / grpc 传输']);
        }
        if ($protocol === 'shadowsocks' && ($security !== 'none' || $network !== 'tcp')) {
            throw ValidationException::withMessages(['protocol' => 'Shadowsocks 仅支持 tcp + 无附加安全层']);
        }

        // Reality 后量子：seed（服务端）与 verify（客户端）成对 —— 只在一边填会让 REALITY
        // 握手后对端拿不到对应公钥材料，直接失败。校验放在建流之前，错误指回表单字段。
        if ($security === 'reality') {
            $seed = trim((string) ($data['mldsa65_seed'] ?? ''));
            $verify = trim((string) ($data['mldsa65_verify'] ?? ''));
            if (($seed === '') !== ($verify === '')) {
                throw ValidationException::withMessages([
                    'mldsa65_seed' => 'mldsa65Seed 与 mldsa65Verify 必须成对设置（同时填写或同时留空）',
                ]);
            }
        }

        // tag：缺省自动建议；唯一性预检（跨节点内唯一）
        $tag = (string) ($data['tag'] ?? '');
        if ($tag === '') {
            $tag = $this->suggestTag($node, $protocol, $security, $network, $existing?->id);
        }
        $dup = $node->xrayInbounds()->where('tag', $tag)
            ->when($existing, fn ($q) => $q->where('id', '!=', $existing->id))
            ->exists();
        if ($dup) {
            throw ValidationException::withMessages(['tag' => "标识 {$tag} 已存在"]);
        }

        // 端口冲突：同节点启用入站不可复用端口（xray 会启动失败）
        $portConflict = $node->xrayInbounds()
            ->where('port', (int) $data['port'])
            ->where('enabled', true)
            ->when($existing, fn ($q) => $q->where('id', '!=', $existing->id))
            ->exists();
        $willEnable = array_key_exists('enabled', $data) ? (bool) $data['enabled'] : ($existing?->enabled ?? true);
        if ($willEnable && $portConflict) {
            throw ValidationException::withMessages(['port' => '端口已被该节点其他启用入站占用']);
        }

        // settings
        $settings = match ($protocol) {
            'vless' => ['decryption' => 'none'],
            'shadowsocks' => array_filter([
                'method' => $data['ss_method'] ?? 'aes-256-gcm',
                'password' => $data['ss_password'] ?? null,
            ], fn ($v) => $v !== null),
            default => [],
        };
        if ($protocol === 'shadowsocks' && empty($settings['password'])) {
            throw ValidationException::withMessages(['ss_password' => 'Shadowsocks 必须设置密码']);
        }

        // stream_settings
        $stream = ['network' => $network, 'security' => $security];

        if ($security === 'reality') {
            $privateKey = (string) ($data['private_key'] ?? '');
            $publicKey = (string) ($data['public_key'] ?? '');

            if ($privateKey === '') {
                // 新建且未提供材料 → 面板自动生成完整一套
                [$privateKey, $publicKey] = RealityKeyService::generateKeypair();
            }
            if ($publicKey === '') {
                $publicKey = $existing !== null
                    ? (string) (($existing->stream_settings['realitySettings']['publicKey'] ?? ''))
                    : '';
            }

            $stream['realitySettings'] = $this->realitySettings($data, $publicKey);
            $inbound->setRealityPrivateKey($privateKey);
        } else {
            $inbound->setRealityPrivateKey(null);
        }

        if ($network === 'ws') {
            $ws = array_filter([
                'path' => (string) ($data['ws_path'] ?? '') !== '' ? (string) $data['ws_path'] : '/',
                'host' => (string) ($data['ws_host'] ?? ''),
            ], fn ($v) => $v !== '');
            $stream['wsSettings'] = $ws;
        } elseif ($network === 'xhttp') {
            $stream['xhttpSettings'] = $this->xhttpSettings($data);
        } elseif ($network === 'httpupgrade') {
            $stream['httpupgradeSettings'] = array_filter([
                'path' => (string) ($data['httpupgrade_path'] ?? '') !== '' ? (string) $data['httpupgrade_path'] : '/',
                'host' => (string) ($data['httpupgrade_host'] ?? ''),
            ], fn ($v) => $v !== '');
        }

        // TCP header 伪装（type=http 时构造 request；响应段用内核默认）
        if ($network === 'tcp' && (string) ($data['tcp_header_type'] ?? 'none') === 'http') {
            $stream['tcpSettings'] = ['header' => [
                'type' => 'http',
                'request' => array_filter([
                    'version' => '1.1',
                    'method' => 'GET',
                    'path' => [(string) ($data['tcp_header_path'] ?? '') !== '' ? (string) $data['tcp_header_path'] : '/'],
                    'headers' => (string) ($data['tcp_header_host'] ?? '') !== '' ? ['Host' => [(string) $data['tcp_header_host']]] : null,
                ], fn ($v) => $v !== null),
            ]];
        }

        if ($security === 'tls') {
            $tls = [];
            if (! empty($data['tls_server_name'])) {
                $tls['serverName'] = (string) $data['tls_server_name'];
            }
            if (! empty($data['tls_certificate']) && ! empty($data['tls_key'])) {
                $tls['certificates'] = [[
                    'certificate' => [(string) $data['tls_certificate']],
                    'key' => [(string) $data['tls_key']],
                ]];
            } else {
                // TLS 入站没有证书时 xray 无法启动；且空 tlsSettings 会编码成 [] 让整份配置加载失败
                throw ValidationException::withMessages(['tls_certificate' => 'TLS 入站必须提供证书与私钥']);
            }
            $stream['tlsSettings'] = $tls;
        }

        $inbound->tag = $tag;
        $inbound->remark = trim((string) ($data['remark'] ?? '')) !== '' ? trim((string) $data['remark']) : null;
        $inbound->protocol = $protocol;
        $inbound->port = (int) $data['port'];
        $inbound->listen = (string) ($data['listen'] ?? '') !== '' ? (string) $data['listen'] : '0.0.0.0';
        $inbound->settings = $settings;
        $inbound->stream_settings = $stream;
        $inbound->sniffing = $this->sniffingSettings($data);
        if (array_key_exists('enabled', $data)) {
            $inbound->enabled = (bool) $data['enabled'];
        }
        if (array_key_exists('sort', $data)) {
            $inbound->sort = (int) $data['sort'];
        }
    }

    /** 非空字符串列表（空则用默认单元素）。 */
    private function stringList(array $values, ?string $default): array
    {
        $out = array_values(array_filter(array_map('strval', $values), fn ($v) => trim($v) !== ''));
        if ($out === [] && $default !== null) {
            $out = [$default];
        }

        return $out;
    }

    /**
     * 嗅探配置（issue #39）：未提交 sniffing 时保持现状默认（存量行为不变）；
     * 默认值不落库（routeOnly=false 省略），与仓库既有「reality 高级字段显式提交才落库」风格一致。
     */
    private function sniffingSettings(array $data): array
    {
        $s = $data['sniffing'] ?? null;
        if (! is_array($s)) {
            return ['enabled' => true, 'destOverride' => ['http', 'tls', 'quic']];
        }

        $config = [
            'enabled' => (bool) ($s['enabled'] ?? true),
            'destOverride' => array_values(array_filter((array) ($s['destOverride'] ?? ['http', 'tls', 'quic']), 'is_string')),
        ];
        if (! empty($s['routeOnly'])) {
            $config['routeOnly'] = true;
        }

        return $config;
    }

    /**
     * REALITY serverNames 可用性过滤（对齐上游 3x-ui filterUsableSANs 口径）：
     * serverNames 是「客户端 SNI 精确匹配白名单」，通配符（*.example.com）与空串不能当
     * SNI 用，直接剔除；过滤后为空则回退默认 SNI。
     */
    private function usableServerNames(array $values): array
    {
        $out = [];
        foreach ($values as $v) {
            $v = trim((string) $v);
            if ($v === '' || str_contains($v, '*')) {
                continue;
            }
            $out[] = $v;
        }

        return array_values(array_unique($out)) ?: [XrayConfigService::DEFAULT_SNI];
    }

    /**
     * REALITY serverNames 与 dest 的一致性拦截（issue #57）。
     *
     * 背景：REALITY 转发时按客户端 SNI 向 dest 取证书，dest 不认的域名必然握手失败；
     * 分派又按 crc32(user_id) 固定，一条坏 SNI 会永久废掉 1/n 的用户（2026-10-22 现场：
     * dest=aws.amazon.com 配了 www.cloudflare.com，106 个用户连不上，重拉订阅无效）。
     *
     * 这里只做**静态**判定（同站点：相等、互为父域，或同注册域），零网络请求 —— 面板视角在国内实测
     * 与节点视角完全不一致（见 agent/reality_scan.go 注释），所以"dest 到底认不认这个 SNI"
     * 只能由节点侧扫描回答（巡检任务 + 前端「检测目标」），保存时不做同步探测。
     *
     * 命中可疑项时要求显式确认（confirm_server_names=1）：绝大多数情况是手滑混进了别家域名，
     * 拦下来；确属同一张证书覆盖的其它站点（CDN 多 SAN）时也能放行，不写死。
     */
    private function guardServerNames(Request $request, array $data, ?XrayInbound $existing = null): void
    {
        $stream = $existing?->stream_settings ?? [];
        $security = (string) ($data['security'] ?? ($stream['security'] ?? ''));
        if ($security !== 'reality') {
            return;
        }

        $dest = (string) ($data['dest'] ?? ($stream['realitySettings']['dest'] ?? ''));
        $host = XrayConfigService::hostOf($dest);
        if ($host === '') {
            return;
        }

        $names = $this->usableServerNames($this->stringList($data['server_names'] ?? [], null));
        $suspect = [];
        foreach ($names as $name) {
            if (! XrayConfigService::sameSite($name, $host)) {
                $suspect[] = $name;
            }
        }

        if ($suspect === [] || $request->boolean('confirm_server_names')) {
            return;
        }

        // 用 HttpException 而不是 ValidationException：本项目把 ValidationException 统一
        // 渲染成 code=500（bootstrap/app.php），前端没法把「需要确认」和真故障分开。
        abort(422, sprintf(
            '这些 serverNames 与 dest（%s）不是同一站点的域名：%s。'
            . 'REALITY 会按客户端 SNI 向 dest 取证书，dest 给不出证书的域名会让被分到的用户全部连不上。'
            . '若确认这些域名由 dest 的同一张证书覆盖（CDN 多 SAN），请确认后再保存。',
            $host,
            implode('、', $suspect)
        ));
    }

    /**
     * 组装 realitySettings：核心四字段 + 3x-ui 高级字段。
     *
     * 高级字段仅在管理员**显式提交**时写入（默认值不落库），老入站 round-trip 不变；
     * 内核默认值统一由 XrayConfigService::realitySettingsFor 在渲染时补齐。
     * 客户端段（fingerprint / spiderX / mldsa65Verify）供订阅链接生成使用。
     */
    private function realitySettings(array $data, string $publicKey): array
    {
        $settings = [
            'dest' => (string) ($data['dest'] ?? '') !== '' ? (string) $data['dest'] : XrayConfigService::DEFAULT_DEST,
            'serverNames' => $this->usableServerNames($this->stringList($data['server_names'] ?? [], null)),
            'shortIds' => $this->stringList($data['short_ids'] ?? [], null) ?: [RealityKeyService::shortId()],
            'publicKey' => $publicKey,
        ];

        // xver/show：REALITY 前置反代场景；均为内核默认值（0/false）时不落库
        if (array_key_exists('xver', $data) && (int) $data['xver'] !== 0) {
            $settings['xver'] = (int) $data['xver'];
        }
        if (array_key_exists('show', $data) && (bool) $data['show']) {
            $settings['show'] = true;
        }

        // 客户端版本窗口 / 时间差：空串与 0 即内核默认
        foreach (['min_client_ver' => 'minClientVer', 'max_client_ver' => 'maxClientVer'] as $in => $out) {
            $v = trim((string) ($data[$in] ?? ''));
            if ($v !== '') {
                $settings[$out] = $v;
            }
        }
        if (array_key_exists('max_timediff', $data) && (int) $data['max_timediff'] !== 0) {
            $settings['maxTimediff'] = (int) $data['max_timediff'];
        }

        // 服务端后量子种子 / 主密钥日志
        $seed = trim((string) ($data['mldsa65_seed'] ?? ''));
        if ($seed !== '') {
            $settings['mldsa65Seed'] = $seed;
        }
        $log = trim((string) ($data['master_key_log'] ?? ''));
        if ($log !== '') {
            $settings['masterKeyLog'] = $log;
        }

        // fallback 限速（整块非零才写）
        foreach (['limit_fallback_upload' => 'limitFallbackUpload', 'limit_fallback_download' => 'limitFallbackDownload'] as $in => $out) {
            $block = $this->limitFallback($data[$in] ?? null);
            if ($block !== null) {
                $settings[$out] = $block;
            }
        }

        // 客户端段（订阅链接：fp / spx / pqv）
        $client = [];
        $fp = trim((string) ($data['fingerprint'] ?? ''));
        if ($fp !== '') {
            $client['fingerprint'] = $fp;
        }
        if (array_key_exists('spider_x', $data) && (string) $data['spider_x'] !== '') {
            $client['spiderX'] = (string) $data['spider_x'];
        }
        $verify = trim((string) ($data['mldsa65_verify'] ?? ''));
        if ($verify !== '') {
            $client['mldsa65Verify'] = $verify;
        }
        if ($client !== []) {
            $settings['settings'] = $client;
        }

        return $settings;
    }

    /** fallback 限速块展示（无则三键全 0，供表单绑定）。 */
    private function presentLimitFallback(mixed $block): array
    {
        $block = is_array($block) ? $block : [];

        return [
            'after_bytes' => (int) ($block['afterBytes'] ?? 0),
            'bytes_per_sec' => (int) ($block['bytesPerSec'] ?? 0),
            'burst_bytes_per_sec' => (int) ($block['burstBytesPerSec'] ?? 0),
        ];
    }

    /** fallback 限速块：三字段全 0（或未提交）返回 null，否则补 0 后返回三键块。 */
    private function limitFallback(?array $data): ?array
    {
        if (! is_array($data)) {
            return null;
        }
        $after = (int) ($data['after_bytes'] ?? 0);
        $rate = (int) ($data['bytes_per_sec'] ?? 0);
        $burst = (int) ($data['burst_bytes_per_sec'] ?? 0);
        if ($after === 0 && $rate === 0 && $burst === 0) {
            return null;
        }

        return [
            'afterBytes' => $after,
            'bytesPerSec' => $rate,
            'burstBytesPerSec' => $burst,
        ];
    }

    /** 建议 tag（协议-安全-传输，冲突自动加序号）。 */
    private function suggestTag(Node $node, string $protocol, string $security, string $network, ?int $excludeId): string
    {
        $base = $protocol;
        if ($security === 'reality') {
            $base .= '-reality';
        } elseif ($security === 'tls' && $network !== 'tcp') {
            $base .= '-ws-tls';
        } elseif ($network !== 'tcp') {
            $base .= '-' . $network;
        }
        $base = preg_replace('/[^A-Za-z0-9_-]/', '', $base) ?: 'inbound';

        $existing = $node->xrayInbounds()
            ->when($excludeId, fn ($q) => $q->where('id', '!=', $excludeId))
            ->pluck('tag')
            ->all();

        $tag = $base;
        $i = 2;
        while (in_array($tag, $existing, true)) {
            $tag = $base . '-' . $i;
            $i++;
        }

        return $tag;
    }

    /** xhttp 高级字段映射（xray-core 键名 => 表单字段）。 */
    private const XHTTP_STRING_FIELDS = [
        'xPaddingBytes' => 'xhttp_padding_bytes',
        'xPaddingKey' => 'xhttp_padding_key',
        'xPaddingHeader' => 'xhttp_padding_header',
        'xPaddingPlacement' => 'xhttp_padding_placement',
        'xPaddingMethod' => 'xhttp_padding_method',
        'sessionIDPlacement' => 'xhttp_session_id_placement',
        'sessionIDKey' => 'xhttp_session_id_key',
        'sessionIDTable' => 'xhttp_session_id_table',
        'sessionIDLength' => 'xhttp_session_id_length',
        'seqPlacement' => 'xhttp_seq_placement',
        'seqKey' => 'xhttp_seq_key',
        'uplinkDataPlacement' => 'xhttp_uplink_data_placement',
        'uplinkDataKey' => 'xhttp_uplink_data_key',
        'scMaxEachPostBytes' => 'xhttp_sc_max_each_post_bytes',
        'scMinPostsIntervalMs' => 'xhttp_sc_min_posts_interval_ms',
        'scStreamUpServerSecs' => 'xhttp_sc_stream_up_server_secs',
        'uplinkHTTPMethod' => 'xhttp_uplink_http_method',
    ];

    private const XHTTP_BOOL_FIELDS = [
        'xPaddingObfsMode' => 'xhttp_padding_obfs_mode',
        'noSSEHeader' => 'xhttp_no_sse_header',
        'noGRPCHeader' => 'xhttp_no_grpc_header',
    ];

    private const XHTTP_INT_FIELDS = [
        'scMaxBufferedPosts' => 'xhttp_sc_max_buffered_posts',
        'serverMaxHeaderBytes' => 'xhttp_server_max_header_bytes',
        'uplinkChunkSize' => 'xhttp_uplink_chunk_size',
    ];

    /**
     * xhttp 传输设置（xray-core SplitHTTPConfig；字段集与 3x-ui xhttp.ts 对齐）。
     * 空值/关/零不落库 —— 未配置即内核默认，避免把默认值写进配置成为 DPI 指纹。
     */
    private function xhttpSettings(array $data): array
    {
        $s = array_filter([
            'path' => (string) ($data['xhttp_path'] ?? '') !== '' ? (string) $data['xhttp_path'] : '/',
            'host' => (string) ($data['xhttp_host'] ?? ''),
            'mode' => (string) ($data['xhttp_mode'] ?? '') !== '' ? (string) $data['xhttp_mode'] : 'auto',
        ], fn ($v) => $v !== '');

        foreach (self::XHTTP_STRING_FIELDS as $key => $field) {
            $v = trim((string) ($data[$field] ?? ''));
            if ($v !== '') {
                $s[$key] = $v;
            }
        }
        foreach (self::XHTTP_BOOL_FIELDS as $key => $field) {
            if (! empty($data[$field])) {
                $s[$key] = true;
            }
        }
        foreach (self::XHTTP_INT_FIELDS as $key => $field) {
            $v = (int) ($data[$field] ?? 0);
            if ($v > 0) {
                $s[$key] = $v;
            }
        }
        $headers = $this->parseHeaderLines((string) ($data['xhttp_headers'] ?? ''));
        if ($headers !== []) {
            $s['headers'] = $headers;
        }

        $xmux = array_filter([
            'maxConcurrency' => trim((string) ($data['xhttp_xmux_max_concurrency'] ?? '')),
            'maxConnections' => trim((string) ($data['xhttp_xmux_max_connections'] ?? '')),
            'cMaxReuseTimes' => trim((string) ($data['xhttp_xmux_c_max_reuse_times'] ?? '')),
            'hMaxRequestTimes' => trim((string) ($data['xhttp_xmux_h_max_request_times'] ?? '')),
            'hMaxReusableSecs' => trim((string) ($data['xhttp_xmux_h_max_reusable_secs'] ?? '')),
            'hKeepAlivePeriod' => trim((string) ($data['xhttp_xmux_h_keep_alive_period'] ?? '')),
        ], fn ($v) => $v !== '');
        if ($xmux !== []) {
            $s['xmux'] = $xmux;
        }

        return $s;
    }

    /** "Key: Value" 行文本 → header map（空行/无冒号行忽略）。 */
    private function parseHeaderLines(string $text): array
    {
        $out = [];
        foreach (preg_split('/\r\n|\r|\n/', $text) ?: [] as $line) {
            $line = trim($line);
            if ($line === '' || ! str_contains($line, ':')) {
                continue;
            }
            [$k, $v] = explode(':', $line, 2);
            $k = trim($k);
            if ($k !== '') {
                $out[$k] = trim($v);
            }
        }

        return $out;
    }

    /** header map → "Key: Value" 行文本（编辑回显；值数组取首个）。 */
    private function headerLinesFrom(mixed $headers): string
    {
        if (! is_array($headers)) {
            return '';
        }
        $lines = [];
        foreach ($headers as $k => $v) {
            $lines[] = $k . ': ' . (is_array($v) ? (string) ($v[0] ?? '') : (string) $v);
        }

        return implode("\n", $lines);
    }

    private function present(XrayInbound $inbound, Collection $users): array
    {
        $stream = $inbound->stream_settings ?? [];
        $reality = $stream['realitySettings'] ?? [];
        $client = is_array($reality['settings'] ?? null) ? $reality['settings'] : [];
        $xs = is_array($stream['xhttpSettings'] ?? null) ? $stream['xhttpSettings'] : [];
        $settings = $inbound->settings ?? [];

        $clientCount = $inbound->protocol === 'shadowsocks'
            ? 0
            : $users->where('protocol', $inbound->protocol)->count();

        return [
            'id' => $inbound->id,
            'tag' => $inbound->tag,
            'remark' => $inbound->remark,
            'protocol' => $inbound->protocol,
            'port' => $inbound->port,
            'listen' => $inbound->listen,
            'network' => $stream['network'] ?? 'tcp',
            'security' => $stream['security'] ?? 'none',
            // 嗅探配置回填（issue #39；存量入站未设过则返回现状默认，编辑表单可直接改）
            'sniffing' => $inbound->sniffing ?: ['enabled' => true, 'destOverride' => ['http', 'tls', 'quic']],
            'dest' => $reality['dest'] ?? null,
            'server_names' => array_values((array) ($reality['serverNames'] ?? [])),
            'short_ids' => array_values((array) ($reality['shortIds'] ?? [])),
            'public_key' => $reality['publicKey'] ?? null,
            'has_private_key' => $inbound->reality_private_key !== null,
            // reality 高级字段（编辑表单回填；mldsa65 为成对材料，随表单 round-trip，
            // 私钥仍只进 config 不外泄）
            'show' => (bool) ($reality['show'] ?? false),
            'xver' => (int) ($reality['xver'] ?? 0),
            'min_client_ver' => (string) ($reality['minClientVer'] ?? ''),
            'max_client_ver' => (string) ($reality['maxClientVer'] ?? ''),
            'max_timediff' => (int) ($reality['maxTimediff'] ?? 0),
            'mldsa65_seed' => (string) ($reality['mldsa65Seed'] ?? ''),
            'mldsa65_verify' => (string) ($client['mldsa65Verify'] ?? ''),
            'master_key_log' => (string) ($reality['masterKeyLog'] ?? ''),
            'fingerprint' => (string) ($client['fingerprint'] ?? ''),
            'spider_x' => (string) ($client['spiderX'] ?? ($reality['spiderX'] ?? '')),
            'limit_fallback_upload' => $this->presentLimitFallback($reality['limitFallbackUpload'] ?? null),
            'limit_fallback_download' => $this->presentLimitFallback($reality['limitFallbackDownload'] ?? null),
            'ws_path' => $stream['wsSettings']['path'] ?? null,
            'ws_host' => $stream['wsSettings']['host'] ?? null,
            'httpupgrade_path' => $stream['httpupgradeSettings']['path'] ?? null,
            'httpupgrade_host' => $stream['httpupgradeSettings']['host'] ?? null,
            'tcp_header_type' => $stream['tcpSettings']['header']['type'] ?? 'none',
            'tcp_header_path' => (string) ($stream['tcpSettings']['header']['request']['path'][0] ?? ''),
            'tcp_header_host' => (string) ($stream['tcpSettings']['header']['request']['headers']['Host'][0]
                ?? ($stream['tcpSettings']['header']['response']['headers']['Host'][0] ?? '')),
            'xhttp_path' => $xs['path'] ?? null,
            'xhttp_host' => $xs['host'] ?? null,
            'xhttp_mode' => $xs['mode'] ?? null,
            'xhttp_padding_bytes' => (string) ($xs['xPaddingBytes'] ?? ''),
            'xhttp_padding_obfs_mode' => (bool) ($xs['xPaddingObfsMode'] ?? false),
            'xhttp_padding_key' => (string) ($xs['xPaddingKey'] ?? ''),
            'xhttp_padding_header' => (string) ($xs['xPaddingHeader'] ?? ''),
            'xhttp_padding_placement' => (string) ($xs['xPaddingPlacement'] ?? ''),
            'xhttp_padding_method' => (string) ($xs['xPaddingMethod'] ?? ''),
            'xhttp_session_id_placement' => (string) ($xs['sessionIDPlacement'] ?? ''),
            'xhttp_session_id_key' => (string) ($xs['sessionIDKey'] ?? ''),
            'xhttp_session_id_table' => (string) ($xs['sessionIDTable'] ?? ''),
            'xhttp_session_id_length' => (string) ($xs['sessionIDLength'] ?? ''),
            'xhttp_seq_placement' => (string) ($xs['seqPlacement'] ?? ''),
            'xhttp_seq_key' => (string) ($xs['seqKey'] ?? ''),
            'xhttp_uplink_data_placement' => (string) ($xs['uplinkDataPlacement'] ?? ''),
            'xhttp_uplink_data_key' => (string) ($xs['uplinkDataKey'] ?? ''),
            'xhttp_uplink_chunk_size' => (int) ($xs['uplinkChunkSize'] ?? 0),
            'xhttp_sc_max_each_post_bytes' => (string) ($xs['scMaxEachPostBytes'] ?? ''),
            'xhttp_sc_max_buffered_posts' => (int) ($xs['scMaxBufferedPosts'] ?? 0),
            'xhttp_sc_min_posts_interval_ms' => (string) ($xs['scMinPostsIntervalMs'] ?? ''),
            'xhttp_sc_stream_up_server_secs' => (string) ($xs['scStreamUpServerSecs'] ?? ''),
            'xhttp_server_max_header_bytes' => (int) ($xs['serverMaxHeaderBytes'] ?? 0),
            'xhttp_uplink_http_method' => (string) ($xs['uplinkHTTPMethod'] ?? ''),
            'xhttp_no_sse_header' => (bool) ($xs['noSSEHeader'] ?? false),
            'xhttp_no_grpc_header' => (bool) ($xs['noGRPCHeader'] ?? false),
            'xhttp_headers' => $this->headerLinesFrom($xs['headers'] ?? []),
            'xhttp_xmux_max_concurrency' => (string) ($xs['xmux']['maxConcurrency'] ?? ''),
            'xhttp_xmux_max_connections' => (string) ($xs['xmux']['maxConnections'] ?? ''),
            'xhttp_xmux_c_max_reuse_times' => (string) ($xs['xmux']['cMaxReuseTimes'] ?? ''),
            'xhttp_xmux_h_max_request_times' => (string) ($xs['xmux']['hMaxRequestTimes'] ?? ''),
            'xhttp_xmux_h_max_reusable_secs' => (string) ($xs['xmux']['hMaxReusableSecs'] ?? ''),
            'xhttp_xmux_h_keep_alive_period' => (string) ($xs['xmux']['hKeepAlivePeriod'] ?? ''),
            'tls_server_name' => $stream['tlsSettings']['serverName'] ?? null,
            'has_tls_cert' => ! empty($stream['tlsSettings']['certificates']),
            'ss_method' => $inbound->protocol === 'shadowsocks' ? ($settings['method'] ?? null) : null,
            'ss_password_set' => $inbound->protocol === 'shadowsocks' && ! empty($settings['password']),
            'enabled' => $inbound->enabled,
            'sort' => $inbound->sort,
            'client_count' => $clientCount,
            'created_at' => $inbound->created_at?->toIso8601String(),
        ];
    }
}
