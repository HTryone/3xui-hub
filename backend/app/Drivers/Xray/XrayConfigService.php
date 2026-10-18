<?php

namespace App\Drivers\Xray;

use App\Models\Node;
use App\Models\User;
use App\Models\XrayBalancer;
use App\Models\XrayInbound;
use App\Models\XrayOutbound;
use App\Models\XrayOutboundSubscription;
use App\Models\XrayRelayClient;
use App\Models\XrayRoutingRule;
use Illuminate\Support\Carbon;
use Illuminate\Support\Collection;
use Illuminate\Support\Facades\DB;

/**
 * xray 节点全量配置生成（双通道的「全量通道」事实源）。
 *
 * 配置内容全部由面板侧管理：
 *   - 入站：xray_inbounds（协议/端口/传输/安全 + 加密 Reality 私钥）
 *   - 出站：内置 direct/blocked + xray_outbounds（含 WARP 生成的 wireguard 出站）
 *   - 路由：内置基础规则（阻私有 / 阻 BT）+ xray_routing_rules
 *   - 用户：按「用户协议 == 入站协议」注入 clients（ss 为单凭据模式，不注入）
 *
 * agent 以 ETag 拉取（无变化 304），仅结构变更时内容才变 —— 用户级操作走
 * adu/rmu 指令通道（NodeApiCommand），不改全量 config。
 *
 * 内核口径（v26.3.27 真机 + 本地 -test 验证）：
 * - freedom 挂 finalRules（block geoip:private + allow）：不依赖 dat 的私网出站兜底
 * - routing 基础规则与 3x-ui 默认一致：geoip:private → blocked、bittorrent → blocked
 * - levels 的数字键必须强转 object（PHP 数字键 json_encode 会变 []，xray 解析报错）
 * - system 段开入/出站统计（出站流量功能的数据源）
 */
class XrayConfigService
{
    /** xray 内核 API（本地回环）监听端口（v26 CLI 必须显式 --server 指向这里）。 */
    public const API_PORT = 10085;

    /** VLESS+Reality 传输流控（v26 校验要求该值，旧写法 xtls-rpra-vision 校验不过）。 */
    public const FLOW = 'xtls-rprx-vision';

    /** 默认 Reality dest / SNI。dest 不能用 TLS 记录 >8192 字节的站（P1 真机踩坑记录）。 */
    public const DEFAULT_DEST = 'www.cloudflare.com:443';
    public const DEFAULT_SNI = 'www.cloudflare.com';

    /** Reality 高级字段缺省语（3x-ui reality.ts 字段集，内核默认值）。 */
    private const REALITY_ADV_DEFAULTS = [
        'xver' => 0,
        'show' => false,
        'minClientVer' => '',
        'maxClientVer' => '',
        'maxTimediff' => 0,
        'mldsa65Seed' => '',
    ];

    // ── 全量渲染 ─────────────────────────────────────────

    /**
     * 渲染全量 config.json（数组形态；stats 用空对象占位，保证 json_encode 出 {}）。
     */
    public function render(Node $node): array
    {
        // 轻量用户行（原生查询，stdClass 数组）：10 万用户从「全模型 538MB」
        // 降到「必要列 ~20MB」。字段由 clientsFor 的兼容访问器读取。
        $users = $this->eligibleUserRows();

        $inbounds = [];
        foreach (XrayInbound::enabledFor($node) as $inbound) {
            $inbounds[] = $this->renderInbound($inbound, $users);
        }

        return array_merge([
            'log' => ['loglevel' => 'warning'],
            'api' => [
                'tag' => 'api',
                'listen' => '127.0.0.1:' . self::API_PORT,
                'services' => ['HandlerService', 'StatsService'],
            ],
            'stats' => new \stdClass(),
            'policy' => $this->policyConfig(),
            'outbounds' => $this->renderOutbounds($node),
            'routing' => $this->renderRouting($node),
            'inbounds' => $inbounds,
        ], $this->renderObservatory($node)); // observatory / burstObservatory（顶层，balancer 需要时）
    }

    /** 渲染并编码为 JSON 字符串（ETag 与响应体的唯一编码口径）。 */
    public function renderJson(Node $node): string
    {
        return json_encode(
            $this->render($node),
            JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE
        );
    }

    /** 全量 config 的 ETag（sha1，agent 用 If-None-Match 对比）。 */
    public function etag(Node $node): string
    {
        return '"' . sha1($this->renderJson($node)) . '"';
    }

    /**
     * 配置指纹（轻量，不渲染）——agent 拉取前的 304 快速判定。
     *
     * 现网按「渲染结果 sha1」判 304，等于每次请求都要全量渲染（10 万用户
     * 7.3s / 538MB）。指纹只查各渲染数据源的 MAX(updated_at)+COUNT(*)，
     * 几毫秒内判断「数据有没有可能变」：相同 → 渲染结果必然相同 → 直接 304。
     *
     * 覆盖 render() 的全部数据源（新增渲染数据源必须同步维护，漏了会
     * 「改了配置不生效」）：
     *   - inbounds / outbounds / outbound_subscriptions / balancers / routing_rules（按节点）
     *   - relay_clients（源节点 + 目标节点两侧）
     *   - users / plans（全局，所有节点共享）
     *   - 节点自身（driver_config 里的路由设置等；config_version 兜结构变更）
     *
     * 流量对账走 increment（不 touch updated_at，渲染内容也不含用量）→ 不触发重渲染。
     */
    public function fingerprint(Node $node): string
    {
        $parts = [(string) $node->xrayConfigVersion()];

        foreach ([
            ['xray_inbounds', 'node_id'],
            ['xray_outbounds', 'node_id'],
            ['xray_outbound_subscriptions', 'node_id'],
            ['xray_balancers', 'node_id'],
            ['xray_routing_rules', 'node_id'],
        ] as [$table, $col]) {
            $parts[] = $this->stamp($table, $col, $node->id);
        }

        // 中转凭据：本节点作为源或目标时都会影响渲染
        $relay = DB::table('xray_relay_clients')
            ->where('node_id', $node->id)
            ->orWhere('source_node_id', $node->id)
            ->selectRaw('MAX(updated_at) AS m, COUNT(*) AS c')
            ->first();
        $parts[] = 'relay:' . ($relay->m ?? '-') . ':' . (int) ($relay->c ?? 0);

        // users / plans：用「内容摘要」而非 MAX(updated_at) —— updated_at 是秒精度，
        // 同一秒内的多次写入（如批量禁用）不会改变 MAX，会漏检「改了配置不生效」。
        // 只拼影响渲染的字段：10 万行全扫约几十 ms，远低于渲染成本（7.3s）。
        $users = DB::table('users')->selectRaw(
            "COUNT(*) AS c, COALESCE(SUM(CRC32(CONCAT_WS('|', id, uuid, protocol, enabled,
                COALESCE(plan_id, '-'), COALESCE(traffic_limit, '-'),
                COALESCE(DATE_FORMAT(expired_at, '%Y-%m-%d %H:%i:%s'), '-'),
                COALESCE(traffic_disabled_at, '-')))), 0) AS s"
        )->first();
        $parts[] = 'users:' . (int) $users->c . ':' . $users->s;

        $plans = DB::table('plans')->selectRaw(
            "COUNT(*) AS c, COALESCE(SUM(CRC32(CONCAT_WS('|', id, type,
                COALESCE(months, '-'), COALESCE(monthly_traffic, '-'),
                COALESCE(period_traffic, '-'), COALESCE(total_traffic, '-')))), 0) AS s"
        )->first();
        $parts[] = 'plans:' . (int) $plans->c . ':' . $plans->s;

        // 节点自身设置：只取真正影响渲染的部分（路由基础设置）。
        // 绝不能含 updated_at / driver_config 全量摘要：agent 心跳（alive）每 5 秒写入
        // xray_version、agent 运行时数据并 touch updated_at —— 那会让指纹永不相等，
        // 节点每轮拉取都拿到 200，误判「配置已更新」并重启内核（v1.33.3 回归）。
        $parts[] = 'nodecfg:' . sha1((string) json_encode($node->xrayRoutingSettings()));

        return sha1(implode('|', $parts));
    }

    private function stamp(string $table, string $col, int $id): string
    {
        $row = DB::table($table)->where($col, $id)->selectRaw('MAX(updated_at) AS m, COUNT(*) AS c')->first();

        return $table . ':' . ($row->m ?? '-') . ':' . (int) ($row->c ?? 0);
    }

    /** 策略段：用户级统计（levels.0）+ 入/出站级统计（system，出站流量数据源）。 */
    public function policyConfig(): array
    {
        return [
            'levels' => (object) [
                '0' => (object) [
                    'statsUserUplink' => true,
                    'statsUserDownlink' => true,
                ],
            ],
            'system' => (object) [
                'statsInboundUplink' => true,
                'statsInboundDownlink' => true,
                'statsOutboundUplink' => true,
                'statsOutboundDownlink' => true,
            ],
        ];
    }

    // ── 入站渲染 ─────────────────────────────────────────

    /**
     * 渲染单个入站片段。
     *
     * @param  Collection  $users  全量候选用户（按协议自动过滤注入 clients）
     * @param  bool  $withStreamSettings  adu 指令载荷不需要 streamSettings（入站已存在）
     */
    public function renderInbound(XrayInbound $inbound, iterable $users, bool $withStreamSettings = true): array
    {
        $settings = $inbound->settings ?? [];

        $clients = $this->clientsFor($inbound, $users);
        if ($clients !== null) {
            // 中转凭据（面板在目标入站上为来源节点自动生成的 client）一并注入；
            // shadowsocks 单凭据模式在上面的分支已返回 null，不走到这里。
            foreach (XrayRelayClient::clientsForInbound($inbound) as $relayClient) {
                $clients[] = $relayClient;
            }
            $settings['clients'] = $clients;
        }

        $frag = [
            'tag' => $inbound->tag,
            'listen' => $inbound->listen ?: '0.0.0.0',
            'port' => $inbound->port,
            'protocol' => $inbound->protocol,
            'settings' => $settings === [] ? new \stdClass() : $settings,
        ];

        if ($withStreamSettings) {
            $stream = $inbound->stream_settings ?? [];
            if (($stream['security'] ?? '') === 'reality') {
                $stream['realitySettings'] = $this->realitySettingsFor($inbound);
            }
            // 空数组子对象必须输出为 {}（PHP 空数组会编码成 []，xray 对 tlsSettings 等
            // 结构体字段报 "cannot unmarshal array into ..." 并拒绝加载整份配置）
            foreach (['tlsSettings', 'realitySettings', 'wsSettings', 'xhttpSettings', 'httpupgradeSettings', 'grpcSettings', 'tcpSettings', 'sockopt'] as $subKey) {
                if (array_key_exists($subKey, $stream) && $stream[$subKey] === []) {
                    $stream[$subKey] = new \stdClass();
                }
            }
            if ($stream !== []) {
                $frag['streamSettings'] = $stream;
            }

            $frag['sniffing'] = $inbound->sniffing ?: [
                'enabled' => true,
                'destOverride' => ['http', 'tls', 'quic'],
            ];
        }

        return $frag;
    }

    /**
     * adu 指令用的入站片段（单用户；不含 streamSettings/私钥 —— 指令会入库）。
     * 格式沿用 P1 真机验证过的契约：{"inbounds":[{完整入站片段, clients:[单用户]}]}。
     */
    public function aduInboundFragment(XrayInbound $inbound, User $user): array
    {
        return $this->renderInbound($inbound, collect([$user]), false);
    }

    /**
     * 多 SNI 入站按用户稳定抽取一个 serverName（订阅链接用）。
     *
     * serverNames 是数组（上限 64），历史上订阅永远只下发 [0]，等于多配的 SNI 全废。
     * 取模 user_id 的好处：同一用户恒定（每次拉订阅都同一个 SNI，不会换着来），
     * 用户之间分散（多 SNI 的意义就是把流量摊到多个目标域名）；
     * 不新增列、不加迁移，单 SNI 入站的行为与取 [0] 完全一致。
     *
     * @param  array<int,mixed>  $serverNames
     */
    public function serverNameFor(array $serverNames, int $userId): string
    {
        $names = [];
        foreach ($serverNames as $name) {
            if (is_string($name) && trim($name) !== '') {
                $names[] = trim($name);
            }
        }

        if ($names === []) {
            return self::DEFAULT_SNI;
        }

        return $names[abs(crc32((string) $userId)) % count($names)];
    }

    /** Reality 安全参数（公开参数来自入站 stream_settings；私钥解密注入）。 */
    public function realitySettingsFor(XrayInbound $inbound): array
    {
        $public = $inbound->stream_settings['realitySettings'] ?? [];

        $settings = [
            'dest' => (string) ($public['dest'] ?? self::DEFAULT_DEST),
            'serverNames' => array_values(array_filter(
                (array) ($public['serverNames'] ?? [self::DEFAULT_SNI]),
                'is_string'
            )),
            'shortIds' => array_values(array_filter(
                (array) ($public['shortIds'] ?? []),
                'is_string'
            )),
        ];

        $privateKey = $inbound->realityPrivateKey();
        if ($privateKey !== null && $privateKey !== '') {
            $settings['privateKey'] = $privateKey;
        }

        // 高级字段：仅写"显式设置且非内核默认"的值。空字符串（minClientVer/
        // maxClientVer/mldsa65Seed 未设置时的形态）与 maxTimediff=0 曾原样写入配置，
        // 实测会让内核把空版本窗当非法认证条件、reality 握手静默拒绝（26.3.27 复现，
        // 换 26.10.10 内核 + 清理空值后恢复）——缺省即不写，与 3x-ui 落库行为对齐。
        foreach (self::REALITY_ADV_DEFAULTS as $key => $default) {
            $value = $public[$key] ?? null;
            if ($value === null || $value === '' || $value === $default) {
                continue;
            }
            $settings[$key] = $value;
        }

        // masterKeyLog / fallback 限速：可选块，仅显式设置时输出
        if (trim((string) ($public['masterKeyLog'] ?? '')) !== '') {
            $settings['masterKeyLog'] = (string) $public['masterKeyLog'];
        }
        foreach (['limitFallbackUpload', 'limitFallbackDownload'] as $key) {
            $block = $public[$key] ?? null;
            if (is_array($block)) {
                $settings[$key] = [
                    'afterBytes' => (int) ($block['afterBytes'] ?? 0),
                    'bytesPerSec' => (int) ($block['bytesPerSec'] ?? 0),
                    'burstBytesPerSec' => (int) ($block['burstBytesPerSec'] ?? 0),
                ];
            }
        }

        return $settings;
    }

    /**
     * 按协议生成 clients 数组；shadowsocks 单凭据模式返回 null（不注入 clients）。
     *
     * @return array<int, array>|null
     */
    public function clientsFor(XrayInbound $inbound, iterable $users): ?array
    {
        if ($inbound->protocol === 'shadowsocks') {
            return null;
        }

        $clients = [];
        foreach ($users as $user) {
            if ((string) $user->protocol !== $inbound->protocol) {
                continue;
            }
            $client = match ($inbound->protocol) {
                'vless' => $this->vlessClient($inbound, $user),
                'vmess' => $this->vmessClient($user),
                'trojan' => $this->trojanClient($user),
                default => null,
            };
            if ($client !== null) {
                $clients[] = $client;
            }
        }

        return $clients;
    }

    private function vlessClient(XrayInbound $inbound, $user): array
    {
        $client = [
            'id' => (string) $user->uuid,
            'email' => $this->clientEmailOf($user),
            'totalGB' => (int) ($user->traffic_limit ?? 0),
            'expiryTime' => $this->expiryZulu($user),
        ];
        if ($this->flowApplies($inbound)) {
            $client['flow'] = self::FLOW;
        }

        return $client;
    }

    private function vmessClient($user): array
    {
        return [
            'id' => (string) $user->uuid,
            'email' => $this->clientEmailOf($user),
            'totalGB' => (int) ($user->traffic_limit ?? 0),
            'expiryTime' => $this->expiryZulu($user),
        ];
    }

    private function trojanClient($user): array
    {
        // trojan 以密码定位用户：统一用 uuid 作密码（面板侧保证唯一）
        return [
            'password' => (string) $user->uuid,
            'email' => $this->clientEmailOf($user),
            'totalGB' => (int) ($user->traffic_limit ?? 0),
            'expiryTime' => $this->expiryZulu($user),
        ];
    }

    /** flow（xtls-rprx-vision）仅适用于 vless + tcp + reality/tls。 */
    public function flowApplies(XrayInbound $inbound): bool
    {
        if ($inbound->protocol !== 'vless') {
            return false;
        }
        $stream = $inbound->stream_settings ?? [];

        return ($stream['network'] ?? 'tcp') === 'tcp'
            && in_array($stream['security'] ?? 'none', ['reality', 'tls'], true);
    }

    // ── 出站渲染 ─────────────────────────────────────────

    /**
     * 出站段：内置 direct（freedom + finalRules 阻私有网段）+ blocked（blackhole），
     * 其后按 sort 追加节点自定义出站，并合并该节点的「出站订阅」出站。
     *
     * 顺序（对齐 3x-ui mergeSubscriptionOutbounds）：
     *   内置 direct/blocked → 订阅 prepend → 自定义出站 → 订阅 append
     * 内置出站恒在最前，保证 direct 作为默认兜底出口不被订阅顶掉。
     * 订阅 tag 与已有 tag 冲突时丢弃（自定义/内置为准），避免 xray 报重复 tag。
     */
    public function renderOutbounds(Node $node): array
    {
        $system = [
            [
                'tag' => 'direct',
                'protocol' => 'freedom',
                'settings' => [
                    'finalRules' => [
                        ['action' => 'block', 'ip' => ['geoip:private']],
                        ['action' => 'allow'],
                    ],
                ],
            ],
            [
                'tag' => 'blocked',
                'protocol' => 'blackhole',
                'settings' => new \stdClass(),
            ],
        ];

        $custom = [];
        $seen = ['direct' => true, 'blocked' => true, 'api' => true];
        foreach (XrayOutbound::enabledFor($node) as $outbound) {
            if (isset($seen[$outbound->tag])) {
                continue; // 系统保留 tag / 重复 tag 不可占用
            }
            $seen[$outbound->tag] = true;

            $frag = [
                'tag' => $outbound->tag,
                'protocol' => $outbound->protocol,
                'settings' => $this->outboundSettings($outbound),
            ];
            $stream = $outbound->stream_settings ?? [];
            // 与入站渲染同守：空数组子对象必须输出 {}（PHP [] 编码成 []，xray 对 tlsSettings
            // 等结构体字段报 "cannot unmarshal array into ..." 拒绝加载整份配置）
            foreach (['tlsSettings', 'realitySettings', 'wsSettings', 'xhttpSettings', 'httpupgradeSettings', 'grpcSettings', 'tcpSettings', 'sockopt'] as $subKey) {
                if (array_key_exists($subKey, $stream) && $stream[$subKey] === []) {
                    $stream[$subKey] = new \stdClass();
                }
            }
            if ($stream !== []) {
                $frag['streamSettings'] = $stream;
            }
            $custom[] = $frag;
        }

        // 订阅出站：prepend 排自定义出站之前、append 排其后；与已有 tag 冲突则丢弃
        $split = XrayOutboundSubscription::activeOutboundsSplit($node);
        $prepend = [];
        foreach ($split['prepend'] as $ob) {
            $tag = (string) ($ob['tag'] ?? '');
            if ($tag === '' || isset($seen[$tag])) {
                continue;
            }
            $seen[$tag] = true;
            $prepend[] = $ob;
        }
        $append = [];
        foreach ($split['append'] as $ob) {
            $tag = (string) ($ob['tag'] ?? '');
            if ($tag === '' || isset($seen[$tag])) {
                continue;
            }
            $seen[$tag] = true;
            $append[] = $ob;
        }

        return array_merge($system, $prepend, $custom, $append);
    }

    /** 出站 settings 组装（wireguard 私钥解密注入；空 settings 出 {}）。 */
    private function outboundSettings(XrayOutbound $outbound): mixed
    {
        $settings = $outbound->settings ?? [];

        if ($outbound->protocol === 'wireguard') {
            $key = $outbound->secretKey();
            if ($key !== null && $key !== '') {
                $settings['secretKey'] = $key;
            }
        }

        return $settings === [] ? new \stdClass() : $settings;
    }

    // ── 路由渲染 ─────────────────────────────────────────

    /**
     * 路由段：内置基础规则（与 3x-ui 默认一致）打头，其后是节点自定义规则。
     */
    public function renderRouting(Node $node): array
    {
        $rules = [
            [
                'type' => 'field',
                'ip' => ['geoip:private'],
                'outboundTag' => 'blocked',
            ],
            [
                'type' => 'field',
                'protocol' => ['bittorrent'],
                'outboundTag' => 'blocked',
            ],
        ];

        // balancer tag 集合：规则目标命中 balancer 时渲染成 balancerTag（而非 outboundTag）
        $balancerTags = XrayBalancer::enabledFor($node)->pluck('tag')->flip();

        // UDP 兼容护栏：xray 内核的 socks / http 出站未实现 UDP（SOCKS5 UDP ASSOCIATE），
        // 被路由进这些出站的 UDP 流量会静默黑洞（实测表现：DNS 全部超时，客户端
        // 「测延迟正常但没网」）。凡自定义规则会把 UDP（network 为空或 =udp）送进
        // TCP-only 出站时，在该规则前注入一条「同匹配条件 + network:udp → direct」
        // 的护栏，UDP 回落直连、TCP 仍按原规则走。balancer 目标不注入（成员出站
        // 是否支持 UDP 未知，不越权改写）。
        $tcpOnlyOutboundTags = XrayOutbound::enabledFor($node)
            ->whereIn('protocol', ['socks', 'http'])
            ->pluck('tag')
            ->flip();

        foreach (XrayRoutingRule::enabledFor($node) as $rule) {
            $frag = ['type' => 'field'];
            if (! empty($rule->domains)) {
                $frag['domain'] = array_values(array_filter((array) $rule->domains, 'is_string'));
            }
            if (! empty($rule->ips)) {
                $frag['ip'] = array_values(array_filter((array) $rule->ips, 'is_string'));
            }
            if (! empty($rule->port)) {
                $frag['port'] = (string) $rule->port;
            }
            if (! empty($rule->network)) {
                $frag['network'] = (string) $rule->network;
            }
            if (! empty($rule->protocol)) {
                $frag['protocol'] = [(string) $rule->protocol];
            }
            if (! empty($rule->inbound_tag)) {
                $frag['inboundTag'] = [(string) $rule->inbound_tag];
            }
            // 空规则（无任何匹配条件）会命中全部流量，跳过防误配（frag 此时只有 type）
            if (count($frag) <= 1) {
                continue;
            }
            // 同一规则既可能指向出站，也可能指向 balancer（tag 全局唯一，哪个在册用哪个）
            $key = $balancerTags->has($rule->outbound_tag) ? 'balancerTag' : 'outboundTag';
            $frag[$key] = $rule->outbound_tag;

            // UDP 兼容护栏（见上方注释）：TCP-only 出站且该规则会吃掉 UDP 流量时，
            // 先出一条同条件的 udp → direct，插在本规则之前，保证 UDP 不黑洞。
            if ($key === 'outboundTag'
                && isset($tcpOnlyOutboundTags[$rule->outbound_tag])
                && ($rule->network === null || $rule->network === 'udp')) {
                $udpFrag = $frag;
                $udpFrag['network'] = 'udp';
                $udpFrag['outboundTag'] = 'direct'; // UDP 回落直连，不进 TCP-only 出站
                $rules[] = $udpFrag;

                // 原规则限定为 tcp（若原本不限网络），避免 udp 分支重复命中本规则
                if ($rule->network === null) {
                    $frag['network'] = 'tcp';
                }
            }

            $rules[] = $frag;
        }

        $settings = $node->xrayRoutingSettings();
        $routing = ['domainStrategy' => $settings['domainStrategy']];
        if ($settings['domainMatcher'] !== '') {
            $routing['domainMatcher'] = $settings['domainMatcher'];
        }

        // 负载均衡器（xray 前缀匹配 selector）；least* 策略需观测器数据，渲染层自动补齐
        $balancers = $this->renderBalancers($node);
        if ($balancers !== []) {
            $routing['balancers'] = $balancers;
        }

        $routing['rules'] = $rules;

        return $routing;
    }

    /**
     * balancer 段渲染。
     *
     * leastPing / leastLoad 及「random|roundRobin + fallbackTag」策略在 xray-core 里
     * 需要观测器数据，否则启动即报 "not all dependencies are resolved"
     * （已用本地 xray 26.3.27 实测）。观测器由 renderObservatory() 据同一批 balancer
     * 在顶层输出，此处只产出 balancers 数组。
     */
    private function renderBalancers(Node $node): array
    {
        $balancers = [];
        foreach (XrayBalancer::enabledFor($node) as $b) {
            $selectors = array_values(array_filter((array) $b->selectors, fn ($s) => is_string($s) && $s !== ''));
            if ($selectors === []) {
                continue; // 无 selector 的 balancer 选不出任何出站，跳过防误配
            }
            $frag = [
                'tag' => $b->tag,
                'selector' => $selectors,
                'strategy' => ['type' => $b->strategy],
            ];
            if (! empty($b->fallback_tag)) {
                $frag['fallbackTag'] = $b->fallback_tag;
            }
            $balancers[] = $frag;
        }

        return $balancers;
    }

    /**
     * 顶层观测器段（observatory / burstObservatory）—— xray 要求它们位于 config 根，
     * 而非 routing 内。据启用的 balancer 策略决定类型与 subjectSelector；无需求时返回 []。
     *
     * @return array{observatory?: array, burstObservatory?: array}
     */
    public function renderObservatory(Node $node): array
    {
        $observatorySelectors = [];   // leastPing → observatory
        $burstSelectors = [];         // leastLoad / roundRobin(+fallback) → burstObservatory

        foreach (XrayBalancer::enabledFor($node) as $b) {
            $selectors = array_values(array_filter((array) $b->selectors, fn ($s) => is_string($s) && $s !== ''));
            if ($selectors === []) {
                continue;
            }
            if ($b->strategy === 'leastPing') {
                $observatorySelectors = array_merge($observatorySelectors, $selectors);
            } elseif ($b->strategy === 'leastLoad'
                || (in_array($b->strategy, ['random', 'roundRobin'], true) && ! empty($b->fallback_tag))) {
                $burstSelectors = array_merge($burstSelectors, $selectors);
            }
        }

        if ($burstSelectors !== []) {
            // 有 burst 需求时，把 leastPing 的 selector 一并并入 burst（避免内核解析到无数据的 observatory）
            return ['burstObservatory' => $this->burstObservatory(
                array_values(array_unique(array_merge($burstSelectors, $observatorySelectors)))
            )];
        }
        if ($observatorySelectors !== []) {
            return ['observatory' => $this->observatory(array_values(array_unique($observatorySelectors)))];
        }

        return [];
    }

    /** xray 主动探测观测器段（leastPing 的延迟数据源）。 */
    private function observatory(array $selectors): array
    {
        return [
            'subjectSelector' => $selectors,
            'probeURL' => 'https://www.google.com/generate_204',
            'probeInterval' => '1m',
            'enableConcurrency' => true,
        ];
    }

    /** xray 突发探测观测器段（leastLoad / roundRobin+fallback 的数据源）。 */
    private function burstObservatory(array $selectors): array
    {
        return [
            'subjectSelector' => $selectors,
            'pingConfig' => [
                'destination' => 'https://www.google.com/generate_204',
                'interval' => '1m',
                'connectivity' => '',
                'timeout' => '5s',
                'sampling' => 2,
                'httpMethod' => 'HEAD',
            ],
        ];
    }

    // ── 用户口径 ─────────────────────────────────────────

    /**
     * 应该存在于节点上的用户（全量 config 的候选集）。
     * 口径与用户级 adu/rmu 一致：有套餐 + 启用 + 未被关闭流量；
     * 协议匹配由 clientsFor 按入站处理（不再限定 vless）。
     */
    public function eligibleUsers(): Collection
    {
        return User::query()
            ->whereNotNull('plan_id')
            ->where('enabled', true)
            ->whereNull('traffic_disabled_at')
            ->get();
    }

    /**
     * 渲染用的轻量用户行（stdClass）：只取 config 实际用到的列。
     *
     * 与 eligibleUsers() 的口径完全一致，区别只在「不构建模型对象」——
     * 10 万用户时全模型约 538MB / 7.3s，轻量行约 20MB。
     * 注意：where 条件必须与 eligibleUsers() 同步维护（present 用模型版）。
     */
    public function eligibleUserRows(): Collection
    {
        return DB::table('users')
            ->select('id', 'uuid', 'email', 'protocol', 'traffic_limit', 'expired_at')
            ->whereNotNull('plan_id')
            ->where('enabled', true)
            ->whereNull('traffic_disabled_at')
            ->get();
    }

    /** 兼容 User 模型与 stdClass 行：客户端 email 标识。 */
    private function clientEmailOf($user): string
    {
        return $user instanceof User ? $user->clientEmail() : 'ch_user_' . $user->id;
    }

    /** 兼容 User 模型（Carbon cast）与 stdClass 行（字符串）：expiryTime 的 ISO Zulu 格式。 */
    private function expiryZulu($user): int|string
    {
        $exp = $user->expired_at ?? null;
        if ($exp === null || $exp === '') {
            return 0;
        }

        return $exp instanceof \DateTimeInterface
            ? $exp->toIso8601ZuluString()
            : Carbon::parse($exp)->toIso8601ZuluString();
    }

    // ── 订阅链接 ─────────────────────────────────────────

    /**
     * 生成用户在节点上的全部订阅 URI（每个协议匹配的启用入站一条）。
     *
     * @return string[]
     */
    public function subscriptionUris(Node $node, User $user): array
    {
        $host = trim((string) $node->host);
        if ($host === '' || (string) $user->uuid === '') {
            return [];
        }

        $uris = [];
        foreach (XrayInbound::enabledFor($node) as $inbound) {
            if ((string) $user->protocol !== $inbound->protocol) {
                continue;
            }
            $uri = match ($inbound->protocol) {
                'vless' => $this->vlessUri($node, $inbound, $user, $host),
                'vmess' => $this->vmessUri($node, $inbound, $user, $host),
                'trojan' => $this->trojanUri($node, $inbound, $user, $host),
                // shadowsocks 是入站级单凭据（密码在入站而非用户），链接对同协议用户通用
                'shadowsocks' => $this->ssUri($node, $inbound, $host),
                default => null,
            };
            if ($uri !== null) {
                $uris[] = $uri;
            }
        }

        return $uris;
    }

    /**
     * 入站显示名（订阅节点名：节点名-tag 区分同节点多入站）。
     *
     * remark 是给人看的中文备注；填了就用它替换 tag 段，没填回落 tag ——
     * 老数据 remark 为 NULL，订阅输出与历史逐字节一致。tag 本身不改（引用型标识）。
     */
    private function inboundLabel(Node $node, XrayInbound $inbound): string
    {
        $remark = trim((string) $inbound->remark);

        return $node->name . '-' . ($remark !== '' ? $remark : $inbound->tag);
    }

    private function vlessUri(Node $node, XrayInbound $inbound, User $user, string $host): string
    {
        $stream = $inbound->stream_settings ?? [];
        $security = $stream['security'] ?? 'none';
        $network = $stream['network'] ?? 'tcp';

        $query = 'type=' . $network . '&security=' . $security;

        if ($security === 'reality') {
            $public = $stream['realitySettings'] ?? [];
            $client = is_array($public['settings'] ?? null) ? $public['settings'] : [];
            // uTLS 指纹：入站未指定则回落 chrome（历史行为）
            $fp = trim((string) ($client['fingerprint'] ?? '')) ?: 'chrome';
            $query .= '&fp=' . rawurlencode($fp);

            $sni = $this->serverNameFor((array) ($public['serverNames'] ?? []), (int) $user->id);
            $query .= '&sni=' . rawurlencode((string) $sni)
                . '&pbk=' . rawurlencode((string) ($public['publicKey'] ?? ''));
            $sid = $public['shortIds'][0] ?? '';
            if ($sid !== '') {
                $query .= '&sid=' . rawurlencode((string) $sid);
            }
            // spiderX：客户端段优先，兼容旧写法落在 realitySettings 顶层
            $spx = (string) ($client['spiderX'] ?? ($public['spiderX'] ?? ''));
            if ($spx !== '') {
                $query .= '&spx=' . rawurlencode($spx);
            }
            // 后量子校验（pqv）
            $pqv = trim((string) ($client['mldsa65Verify'] ?? ''));
            if ($pqv !== '') {
                $query .= '&pqv=' . rawurlencode($pqv);
            }
            if ($this->flowApplies($inbound)) {
                $query .= '&flow=' . self::FLOW;
            }
        } elseif ($security === 'tls') {
            $tls = $stream['tlsSettings'] ?? [];
            $query .= '&fp=chrome&sni=' . rawurlencode((string) ($tls['serverName'] ?? $host));
        } else {
            $query .= '&fp=chrome';
        }

        if ($network === 'ws') {
            $ws = $stream['wsSettings'] ?? [];
            $query .= '&path=' . rawurlencode((string) ($ws['path'] ?? '/'));
            if (! empty($ws['host'])) {
                $query .= '&host=' . rawurlencode((string) $ws['host']);
            }
        } elseif ($network === 'httpupgrade') {
            $hu = $stream['httpupgradeSettings'] ?? [];
            $query .= '&path=' . rawurlencode((string) ($hu['path'] ?? '/'));
            if (! empty($hu['host'])) {
                $query .= '&host=' . rawurlencode((string) $hu['host']);
            }
        } elseif ($network === 'xhttp') {
            // 与出站链接生成器同源：xhttp 参数（含高级字段 extra / x_padding_bytes）
            $xhttp = is_array($stream['xhttpSettings'] ?? null) ? $stream['xhttpSettings'] : [];
            $query .= OutboundLinkGenerator::xhttpQuery($xhttp);
        }

        if ($network === 'tcp' && ($stream['tcpSettings']['header']['type'] ?? '') === 'http') {
            // TCP header 伪装（与 3x-ui 订阅口径一致：request.path + Host，response 优先）
            $header = $stream['tcpSettings']['header'];
            $query .= '&headerType=http&path=' . rawurlencode((string) ($header['request']['path'][0] ?? '/'));
            $headerHost = (string) ($header['response']['headers']['Host'][0] ?? ($header['request']['headers']['Host'][0] ?? ''));
            if ($headerHost !== '') {
                $query .= '&host=' . rawurlencode($headerHost);
            }
        }

        return sprintf(
            'vless://%s@%s:%d?%s#%s',
            $user->uuid,
            $host,
            $inbound->port,
            $query,
            rawurlencode($this->inboundLabel($node, $inbound))
        );
    }

    private function vmessUri(Node $node, XrayInbound $inbound, User $user, string $host): string
    {
        $stream = $inbound->stream_settings ?? [];
        $network = $stream['network'] ?? 'tcp';
        $security = $stream['security'] ?? 'none';
        $ws = $stream['wsSettings'] ?? [];
        $tls = $stream['tlsSettings'] ?? [];

        $payload = [
            'v' => '2',
            'ps' => $this->inboundLabel($node, $inbound),
            'add' => $host,
            'port' => (string) $inbound->port,
            'id' => (string) $user->uuid,
            'aid' => '0',
            'scy' => 'auto',
            'net' => $network,
            'type' => 'none',
            'host' => (string) ($ws['host'] ?? ''),
            'path' => (string) ($ws['path'] ?? ''),
            'tls' => $security === 'tls' ? 'tls' : '',
            'sni' => $security === 'tls' ? (string) ($tls['serverName'] ?? $host) : '',
        ];

        return 'vmess://' . base64_encode(
            (string) json_encode($payload, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES)
        );
    }

    private function trojanUri(Node $node, XrayInbound $inbound, User $user, string $host): string
    {
        $stream = $inbound->stream_settings ?? [];
        $network = $stream['network'] ?? 'tcp';
        $security = $stream['security'] ?? 'none';
        $tls = $stream['tlsSettings'] ?? [];

        $query = 'security=' . ($security === 'tls' ? 'tls' : 'none') . '&type=' . $network;
        if ($security === 'tls') {
            $query .= '&sni=' . rawurlencode((string) ($tls['serverName'] ?? $host));
        }
        if ($network === 'ws') {
            $ws = $stream['wsSettings'] ?? [];
            $query .= '&path=' . rawurlencode((string) ($ws['path'] ?? '/'));
        }

        return sprintf(
            'trojan://%s@%s:%d?%s#%s',
            $user->uuid,
            $host,
            $inbound->port,
            $query,
            rawurlencode($this->inboundLabel($node, $inbound))
        );
    }

    /** Shadowsocks 链接（SIP002）：密码在入站侧，同协议用户共用同一链接。 */
    private function ssUri(Node $node, XrayInbound $inbound, string $host): string
    {
        $settings = $inbound->settings ?? [];
        $method = (string) ($settings['method'] ?? 'aes-256-gcm');
        $password = (string) ($settings['password'] ?? '');

        return sprintf(
            'ss://%s@%s:%d#%s',
            base64_encode($method . ':' . $password),
            $host,
            $inbound->port,
            rawurlencode($this->inboundLabel($node, $inbound))
        );
    }
}
