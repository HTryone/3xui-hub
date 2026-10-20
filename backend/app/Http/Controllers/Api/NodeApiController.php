<?php

namespace App\Http\Controllers\Api;

use App\Drivers\Xray\XrayConfigService;
use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Models\SiteConfig;
use App\Services\NodeApiCommandService;
use App\Services\TrafficSyncService;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Carbon;
use Illuminate\Support\Facades\Cache;
use Illuminate\Support\Facades\Log;

/**
 * xray 节点 API（/api/node-api/*，node.auth 中间件按节点密钥鉴权）。
 *
 * 六个端点（agent 全程只出连）：
 *   POST /register  注册：上报 Reality 公/私钥、SNI、内核版本 → 写 driver_config、节点转在线
 *   GET  /config    全量配置 + ETag（无变化 304；结构变更的兜底通道）
 *   POST /push      流量绝对值（agent 本地 statsquery 聚合）→ 复用 TrafficSyncService 增量落账
 *   POST /alive     心跳 + 资源 + config_version 回执（滞后检测）；回执里带上最新版本号
 *   POST /execute   指令长轮询（≤20s 挂起；取走 pending 指令即原子认领，防多取）
 *   POST /ack       指令执行回执（success/failed + 输出摘录）
 */
class NodeApiController extends Controller
{
    /** execute 长轮询挂起上限（秒）：保持在常见 PHP max_execution_time(30s) 之内。 */
    private const EXECUTE_WAIT_SECONDS = 20;

    /** push 单次接受的流量用户数上限（与 NodeWsServer::MAX_STATS_PER_REPORT 一致）。 */
    private const MAX_STATS_PER_PUSH = 2000;

    public function __construct(
        private readonly XrayConfigService $config,
    ) {}

    // ── POST /api/node-api/register ──────────────────────────

    /**
     * 注册：只负责"连上"（上报版本 / 主机信息）。
     *
     * 业务材料（Reality 密钥、入站协议等）全部由面板侧管理 —— 注册不产生
     * 任何配置内容，因此这里不递增 config_version（修复 P1 注册即 bump
     * 导致的「滞后虚警」：节点版本永远追不上面板版本号）。
     */
    public function register(Request $request): JsonResponse
    {
        $node = $this->node($request);

        $data = $request->validate([
            'xray_version' => ['nullable', 'string', 'max:64'],
            'agent_version' => ['nullable', 'string', 'max:64'],
            'kernel_arch' => ['nullable', 'string', 'max:32'],
            'hostname' => ['nullable', 'string', 'max:128'],
        ]);

        $cfg = [];
        if (! empty($data['xray_version'])) {
            $cfg['xray_version'] = (string) $data['xray_version'];
        }
        if (! empty($data['agent_version'])) {
            $cfg['agent_version'] = (string) $data['agent_version'];
        }
        if (! empty($data['kernel_arch'])) {
            $cfg['kernel_arch'] = (string) $data['kernel_arch'];
        }
        if (! empty($data['hostname'])) {
            $cfg['hostname'] = (string) $data['hostname'];
        }
        // agent 刚从重启流程进来，WS 尚未连接（连上后由 WS 服务写 true）
        $cfg['ws'] = ['connected' => false, 'at' => now()->toIso8601String()];

        // 只合并这几个键：整块回写 driver_config 会把并发 bump 的 config_version 覆盖回旧值
        $node->mergeDriverConfig($cfg);

        // 连接地址自动回填：xray 节点的 host 为空时，用 agent 本次请求的来源公网
        // IP 预填 —— 订阅链接依赖 host 生成，空值会让用户订阅静默为空。
        // 仅回填公网地址：内网/回环（面板与节点同机/同内网）无法用于客户端连接，
        // 保持为空、由订阅侧给出诊断提示；管理员随时可在节点编辑中覆盖。
        if ($node->isXray() && (string) $node->host === '') {
            $ip = (string) $request->ip();
            if ($ip !== '' && filter_var($ip, FILTER_VALIDATE_IP, FILTER_FLAG_NO_PRIV_RANGE | FILTER_FLAG_NO_RES_RANGE) !== false) {
                $node->host = $ip;
            }
        }

        $node->forceFill([
            'status' => 'online',
            'last_check_at' => now(),
        ])->save();

        return response()->json(['code' => 0, 'data' => [
            'node_name' => $node->name,
            'config_version' => $node->xrayConfigVersion(),
            'pull_interval' => $this->pullInterval(),
            'ws' => $this->wsInfo($request),
        ]]);
    }

    // ── GET /api/node-api/config ─────────────────────────────

    public function config(Request $request): \Symfony\Component\HttpFoundation\Response
    {
        $node = $this->node($request);

        // 轻量指纹判 304：不再每次先全量渲染（10 万用户 7.3s / 538MB）。
        // 指纹覆盖 render() 的全部数据源（见 XrayConfigService::fingerprint）：
        // 相同 ⇒ 数据没变 ⇒ 渲染结果必然相同 ⇒ 直接 304；指纹变了才走渲染。
        $etag = '"' . $this->config->fingerprint($node) . '"';

        // If-None-Match 按 RFC 7232 弱比较：CDN（Cloudflare 等）压缩响应时会把强 ETag
        // 改写为弱 ETag（W/"…"），而节点 agent 的 Go http 客户端默认带 Accept-Encoding:
        // gzip，收到的永远是弱 ETag —— 只做强比较会永不匹配、永远回 200 全量配置，
        // agent 每轮误判「配置已更新」并重启内核。两侧各剥掉可选的 W/ 前缀与引号后再比。
        $normalizeEtag = static fn (string $v): string => trim(preg_replace('/^W\//i', '', trim($v)) ?? '', '"');

        $ifNoneMatch = trim((string) $request->header('If-None-Match', ''));
        if ($ifNoneMatch !== '' && $normalizeEtag($ifNoneMatch) === $normalizeEtag($etag)) {
            return response('', 304, ['ETag' => $etag]);
        }

        // 指纹变了（或客户端首次拉取）→ 完整渲染
        $json = $this->config->renderJson($node);

        // 手工拼包：config 内 "stats": {} 必须保持空对象（json_encode 数组会变 []，
        // xray 解析 [] 会报错），所以 config 部分直接嵌入原始 JSON 字符串。
        $payload = '{"code":0,"data":{"config":' . $json
            . ',"config_version":' . $node->xrayConfigVersion()
            . ',"pull_interval":' . $this->pullInterval() . '}}';

        return response($payload, 200, [
            'Content-Type' => 'application/json; charset=utf-8',
            'ETag' => $etag,
        ]);
    }

    // ── POST /api/node-api/push ──────────────────────────────

    public function push(Request $request): JsonResponse
    {
        $node = $this->node($request);

        // 超过单次上限时接口本来就会 422，但错误信息只是字段校验文案，看不懂「流量没落账」。
        // 这里显式拦一道：写一条（节点级节流的）error 日志 + 返回人话错误，让问题可见。
        $rawStats = $request->input('stats');
        if (is_array($rawStats) && count($rawStats) > self::MAX_STATS_PER_PUSH) {
            if (Cache::add('node-api-push-overflow:' . $node->id, 1, 60)) {
                Log::error(
                    '[node-api] node#' . $node->id . ' push 上报 ' . count($rawStats) . ' 条流量，超过单次上限 '
                    . self::MAX_STATS_PER_PUSH . '，已拒绝：这些流量不会落账（需改为只上报有变化的用户）'
                );
            }

            return response()->json([
                'code' => 42201,
                'msg' => 'stats 超过单次上限 ' . self::MAX_STATS_PER_PUSH . ' 条，请只上报有变化的用户',
                'data' => null,
            ], 422);
        }

        $data = $request->validate([
            'stats' => ['required', 'array', 'max:' . self::MAX_STATS_PER_PUSH],
            'stats.*.up' => ['nullable', 'integer', 'min:0'],
            'stats.*.down' => ['nullable', 'integer', 'min:0'],
        ]);

        $stats = [];
        foreach ($data['stats'] as $email => $pair) {
            if (! is_string($email) || ! is_array($pair)) {
                continue;
            }
            $stats[$email] = [
                'up' => (int) ($pair['up'] ?? 0),
                'down' => (int) ($pair['down'] ?? 0),
            ];
        }

        // 节点锁内读+提交，幂等增量；与 3x-ui 路径共用同一段落账实现
        $result = app(TrafficSyncService::class)->syncNodeFromSource($node, fn () => $stats);

        return response()->json(['code' => 0, 'data' => [
            'synced' => (bool) ($result['acquired'] ?? false),
            'delta_users' => count($result['deltaMap'] ?? []),
        ]]);
    }

    // ── POST /api/node-api/alive ─────────────────────────────

    public function alive(Request $request): JsonResponse
    {
        $node = $this->node($request);

        $data = $request->validate([
            'xray_version' => ['nullable', 'string', 'max:64'],
            'config_version' => ['nullable', 'integer', 'min:0'],
            'cpu' => ['nullable', 'numeric', 'min:0', 'max:100'],
            'mem' => ['nullable', 'numeric', 'min:0', 'max:100'],
            'uptime' => ['nullable', 'integer', 'min:0'],
            'conns' => ['nullable', 'integer', 'min:0'],
            'active_users' => ['nullable', 'integer', 'min:0'],
            'up_speed' => ['nullable', 'numeric', 'min:0'],
            'down_speed' => ['nullable', 'numeric', 'min:0'],
            'net_total_up' => ['nullable', 'integer', 'min:0'],
            'net_total_down' => ['nullable', 'integer', 'min:0'],
        ]);

        $cfg = [
            'agent' => [
                'cpu' => isset($data['cpu']) ? (float) $data['cpu'] : null,
                'mem' => isset($data['mem']) ? (float) $data['mem'] : null,
                'uptime' => isset($data['uptime']) ? (int) $data['uptime'] : null,
                'conns' => isset($data['conns']) ? (int) $data['conns'] : null,
                'active_users' => isset($data['active_users']) ? (int) $data['active_users'] : null,
                'up_speed' => isset($data['up_speed']) ? (float) $data['up_speed'] : null,
                'down_speed' => isset($data['down_speed']) ? (float) $data['down_speed'] : null,
                // 机器网卡累计收发：agent 读不到 /proc/net/dev（非 Linux）时不上报 → null，不当 0
                'net_total_up' => isset($data['net_total_up']) ? (int) $data['net_total_up'] : null,
                'net_total_down' => isset($data['net_total_down']) ? (int) $data['net_total_down'] : null,
                'at' => now()->toIso8601String(),
            ],
        ];

        if (! empty($data['xray_version'])) {
            $cfg['xray_version'] = (string) $data['xray_version'];
        }

        // 配置滞后：agent 报的版本落后于面板当前版本 → 记录「落后了多久」（自本地 config 变更时刻算起）
        $current = $node->xrayConfigVersion();
        $reported = (int) ($data['config_version'] ?? 0);
        if ($reported < $current) {
            $changedAt = $node->driver_config['config_changed_at'] ?? null;
            $cfg['lag_seconds'] = $changedAt
                ? max(0, (int) Carbon::parse($changedAt)->diffInSeconds(now()))
                : 0;
        } else {
            $cfg['lag_seconds'] = 0;
        }

        // 只合并运行时键：整块回写会把并发 bumpXrayConfigVersion() 的版本号覆盖回旧值，
        // 指纹随之回退 → agent 拿到内容相同的 200 再重启一次内核。
        $node->mergeDriverConfig($cfg);

        $node->forceFill([
            'status' => 'online',
            'last_check_at' => now(),
        ])->save();

        return response()->json(['code' => 0, 'data' => [
            'config_version' => $node->xrayConfigVersion(),
            'pull_interval' => $this->pullInterval(),
        ]]);
    }

    // ── POST /api/node-api/execute（长轮询） ───────────────────

    public function execute(Request $request): JsonResponse
    {
        $node = $this->node($request);
        $deadline = microtime(true) + self::EXECUTE_WAIT_SECONDS;

        do {
            // 原子认领（pending → acknowledged）：与 WS 推送通道共用同一实现，
            // 并发下同一条指令不会被两条通道重复下发
            $commands = app(NodeApiCommandService::class)->claimPending($node);
            if ($commands !== []) {
                return response()->json(['code' => 0, 'data' => ['commands' => $commands]]);
            }

            sleep(2);
        } while (microtime(true) < $deadline);

        return response()->json(['code' => 0, 'data' => ['commands' => []]]);
    }

    // ── POST /api/node-api/ack ───────────────────────────────

    public function ack(Request $request): JsonResponse
    {
        $node = $this->node($request);

        $data = $request->validate([
            'results' => ['required', 'array', 'max:50'],
            'results.*.id' => ['required', 'integer'],
            'results.*.success' => ['required', 'boolean'],
            // 放宽到 40000：reality_scan 的 output 是一整份结果 JSON（落库侧另按 action 截断）
            'results.*.output' => ['nullable', 'string', 'max:40000'],
        ]);

        // 与 WS 通道共用同一回执实现（防两条通道状态口径漂移）
        $updated = app(NodeApiCommandService::class)->applyAck($node, $data['results']);

        return response()->json(['code' => 0, 'data' => ['updated' => $updated]]);
    }

    // ── helpers ─────────────────────────────────────────────

    /** node.auth 中间件已验证并挂上的节点。 */
    private function node(Request $request): Node
    {
        /** @var Node $node */
        $node = $request->attributes->get('node');

        return $node;
    }

    /** 配置拉取周期（秒）：admin 设置项 xray_pull_interval，缺省 30。 */
    private function pullInterval(): int
    {
        $value = (int) SiteConfig::getValue('xray_pull_interval', '30');

        return $value >= 5 ? $value : 30;
    }

    /**
     * 节点 WS 连接信息（register 下发给 agent；关闭时 agent 走 HTTP 降级通道）。
     *
     * scheme 判断双保险：Laravel 的 isSecure（受 TrustProxies 影响）+
     * nginx 普遍注入的 X-Forwarded-Proto（install.sh 已补该头）。
     * env XRAY_WS_PUBLIC_URL 可整体覆盖（本地联调直连 8091 等场景）。
     */
    private function wsInfo(Request $request): array
    {
        if (! (bool) config('xray.ws_enabled', true)) {
            return ['enabled' => false, 'url' => ''];
        }

        $override = (string) config('xray.ws_public_url', '');
        if ($override !== '') {
            return ['enabled' => true, 'url' => $override];
        }

        $secure = $request->isSecure()
            || strtolower((string) $request->header('X-Forwarded-Proto', '')) === 'https';

        return [
            'enabled' => true,
            'url' => ($secure ? 'wss' : 'ws') . '://' . $request->getHost() . (string) config('xray.ws_path', '/node-ws'),
        ];
    }
}
