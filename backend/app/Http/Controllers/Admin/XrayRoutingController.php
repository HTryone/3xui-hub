<?php

namespace App\Http\Controllers\Admin;

use App\Drivers\Xray\XrayConfigService;
use App\Drivers\Xray\XrayRelayService;
use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Models\XrayBalancer;
use App\Models\XrayInbound;
use App\Models\XrayOutbound;
use App\Models\XrayOutboundSubscription;
use App\Models\XrayRoutingRule;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Illuminate\Validation\ValidationException;

/**
 * Admin xray 节点路由管理（/admin-api/nodes/{node}/xray-routing）。
 *
 * - 规则 CRUD（域名/IP/端口/网络/协议/入站 → 目标出站 / balancer）
 * - 负载均衡器（balancer）CRUD：selector 前缀 → 多个出站间分摊
 * - 基础设置（routing.domainStrategy / domainMatcher）存取
 * - 路由测试器：输入域名或 IP，按渲染口径（内置基础规则 + 自定义规则）
 *   逐条模拟匹配，返回命中的规则与最终出站
 *
 * 任何变更后 bump 该节点 config_version → 节点秒级拉取生效。
 */
class XrayRoutingController extends Controller
{
    use ApiResponse;

    /** routing.domainStrategy 合法值（xray routing 枚举）。 */
    public const DOMAIN_STRATEGIES = ['AsIs', 'IPIfNonMatch', 'IPOnDemand'];

    /** routing.domainMatcher 合法值（空=内核默认）。 */
    public const DOMAIN_MATCHERS = ['', 'mph', 'hybrid'];

    public function __construct(
        private readonly XrayConfigService $config,
        private readonly XrayRelayService $relay,
    ) {}

    /** GET /admin-api/nodes/{node}/xray-routing */
    public function index(Node $node): JsonResponse
    {
        $this->ensureXray($node);

        $rules = $node->xrayRoutingRules()->orderBy('sort')->orderBy('id')->get();
        $balancers = $node->xrayBalancers()->orderBy('sort')->orderBy('id')->get();

        return $this->success([
            'rules' => $rules->map(fn (XrayRoutingRule $r) => $this->present($r))->values()->all(),
            'balancers' => $balancers->map(fn (XrayBalancer $b) => $this->presentBalancer($b))->values()->all(),
            // 规则目标候选：仅本节点已有出站 + 已启用 balancer（tag 全局唯一）。
            // 「本站服务器」中转出口由出站页创建（见 XrayOutboundController::storeRelay），
            // 路由页只引用已建出的中转出站，不再生成 relay 候选。
            'outbound_tags' => $this->targetTags($node),
            'outbound_only_tags' => $this->outboundTags($node),
            // 来源入站候选：对象数组（供前端显示「端口 · 协议 · tag」；value 仍为 tag）
            'inbounds' => $this->inboundCandidates($node),
            'settings' => $node->xrayRoutingSettings(),
            'strategies' => XrayBalancer::STRATEGIES,
            'domain_strategies' => self::DOMAIN_STRATEGIES,
            'domain_matchers' => self::DOMAIN_MATCHERS,
            'config_version' => $node->xrayConfigVersion(),
            // 实际下发到节点的 routing.rules（含渲染层自动注入的护栏，如 socks/http
            // 出站的 udp→direct）。护栏不入库，规则列表里看不到；这里透出真实
            // 渲染结果供前端「下发预览」展示，避免「页面没变化 = 没生效」误判。
            'render_preview' => [
                'rules' => $this->config->renderRouting($node)['rules'],
                'generated_at' => now()->toIso8601String(),
            ],
        ]);
    }

    /** POST /admin-api/nodes/{node}/xray-routing */
    public function store(Request $request, Node $node): JsonResponse
    {
        $this->ensureXray($node);
        $data = $this->validatePayload($request, $node);

        $rule = new XrayRoutingRule();
        $rule->node_id = $node->id;
        $this->fillFromPayload($rule, $data);
        $rule->save();

        $node->bumpXrayConfigVersion();

        return $this->success([
            'rule' => $this->present($rule->fresh()),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** PUT /admin-api/nodes/{node}/xray-routing/{rule} */
    public function update(Request $request, Node $node, XrayRoutingRule $rule): JsonResponse
    {
        $this->ensureXray($node);
        $this->ensureOwned($node, $rule);

        $data = $this->validatePayload($request, $node);
        $this->fillFromPayload($rule, $data);
        $rule->save();

        $node->bumpXrayConfigVersion();

        return $this->success([
            'rule' => $this->present($rule->fresh()),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** DELETE /admin-api/nodes/{node}/xray-routing/{rule} */
    public function destroy(Node $node, XrayRoutingRule $rule): JsonResponse
    {
        $this->ensureXray($node);
        $this->ensureOwned($node, $rule);

        $rule->delete();

        // 回收：该规则引用的自动中转出站若无其它规则再引用 → 删出站 + 清对方承接凭据
        $this->relay->cleanupOutboundForRule($node, $rule);

        $node->bumpXrayConfigVersion();

        return $this->success(['config_version' => $node->xrayConfigVersion()]);
    }

    // ── 负载均衡器（balancer） ────────────────────────────

    /** POST /admin-api/nodes/{node}/xray-routing/balancers */
    public function storeBalancer(Request $request, Node $node): JsonResponse
    {
        $this->ensureXray($node);
        $data = $this->validateBalancer($request, $node);

        $balancer = new XrayBalancer();
        $balancer->node_id = $node->id;
        $this->fillBalancer($balancer, $data);
        $balancer->save();

        $node->bumpXrayConfigVersion();

        return $this->success([
            'balancer' => $this->presentBalancer($balancer->fresh()),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** PUT /admin-api/nodes/{node}/xray-routing/balancers/{balancer} */
    public function updateBalancer(Request $request, Node $node, XrayBalancer $balancer): JsonResponse
    {
        $this->ensureXray($node);
        if ((int) $balancer->node_id !== (int) $node->id) {
            abort(404);
        }

        $data = $this->validateBalancer($request, $node, $balancer);
        $this->fillBalancer($balancer, $data);
        $balancer->save();

        $node->bumpXrayConfigVersion();

        return $this->success([
            'balancer' => $this->presentBalancer($balancer->fresh()),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** DELETE /admin-api/nodes/{node}/xray-routing/balancers/{balancer} */
    public function destroyBalancer(Node $node, XrayBalancer $balancer): JsonResponse
    {
        $this->ensureXray($node);
        if ((int) $balancer->node_id !== (int) $node->id) {
            abort(404);
        }

        $balancer->delete();
        $node->bumpXrayConfigVersion();

        return $this->success(['config_version' => $node->xrayConfigVersion()]);
    }

    // ── 基础设置（domainStrategy / domainMatcher） ────────

    /** PUT /admin-api/nodes/{node}/xray-routing/settings */
    public function updateSettings(Request $request, Node $node): JsonResponse
    {
        $this->ensureXray($node);

        $data = $request->validate([
            'domain_strategy' => ['nullable', 'string', 'in:' . implode(',', self::DOMAIN_STRATEGIES)],
            'domain_matcher' => ['nullable', 'string', 'in:' . implode(',', array_filter(self::DOMAIN_MATCHERS, fn ($m) => $m !== ''))],
        ]);

        $node->setXrayRoutingSettings(
            (string) ($data['domain_strategy'] ?? 'AsIs'),
            (string) ($data['domain_matcher'] ?? ''),
        );
        $node->bumpXrayConfigVersion();

        return $this->success([
            'settings' => $node->xrayRoutingSettings(),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /**
     * POST /admin-api/nodes/{node}/xray-routing/test
     * body: {domain?: "...", ip?: "..."} → 按渲染口径逐条模拟匹配。
     */
    public function test(Request $request, Node $node): JsonResponse
    {
        $this->ensureXray($node);

        $data = $request->validate([
            'domain' => ['nullable', 'string', 'max:255'],
            'ip' => ['nullable', 'string', 'max:64'],
        ]);

        $domain = trim((string) ($data['domain'] ?? ''));
        $ip = trim((string) ($data['ip'] ?? ''));
        if ($domain === '' && $ip === '') {
            return $this->error('请输入域名或 IP');
        }

        // 渲染口径的完整规则链（内置基础规则 + 自定义）
        $rules = $this->config->renderRouting($node)['rules'];
        $matched = null;

        foreach ($rules as $idx => $rule) {
            $hit = false;
            $notes = [];

            if ($domain !== '' && ! empty($rule['domain'])) {
                foreach ((array) $rule['domain'] as $pattern) {
                    $result = $this->matchDomain((string) $pattern, $domain);
                    if ($result === true) {
                        $hit = true;
                        $notes[] = "域名命中 {$pattern}";
                        break;
                    }
                    if ($result === null) {
                        $notes[] = "规则含 {$pattern}（需内核 geo 数据，无法在面板精确判定）";
                    }
                }
            }

            if (! $hit && $ip !== '' && ! empty($rule['ip'])) {
                foreach ((array) $rule['ip'] as $pattern) {
                    $result = $this->matchIp((string) $pattern, $ip);
                    if ($result === true) {
                        $hit = true;
                        $notes[] = "IP 命中 {$pattern}";
                        break;
                    }
                    if ($result === null) {
                        $notes[] = "规则含 {$pattern}（需内核 geo 数据，无法在面板精确判定）";
                    }
                }
            }

            if ($hit) {
                $matched = [
                    'index' => $idx,
                    'rule' => $rule,
                    // 规则目标是 balancer 时只有 balancerTag（最终出口由选举决定）
                    'outbound' => $rule['balancerTag'] ?? $rule['outboundTag'] ?? null,
                    'is_balancer' => isset($rule['balancerTag']),
                    'notes' => $notes,
                ];
                break;
            }
        }

        return $this->success([
            'matched' => $matched,
            // 未命中任何规则：走第一条出站（xray 默认行为 = direct）
            'outbound' => $matched['outbound'] ?? 'direct',
            // 默认出口（规则链未命中时的兜底），供前端显式展示「最终出口从何而来」
            'default_outbound' => 'direct',
            'rules_checked' => count($rules),
        ]);
    }

    // ── 内部 ─────────────────────────────────────────────

    private function ensureXray(Node $node): void
    {
        if (! $node->isXray()) {
            abort(422, '仅 xray 节点支持该操作');
        }
    }

    private function ensureOwned(Node $node, XrayRoutingRule $rule): void
    {
        if ((int) $rule->node_id !== (int) $node->id) {
            abort(404);
        }
    }

    /** 可选目标出站：内置 + 自定义 + warp + 订阅。 */
    private function outboundTags(Node $node): array
    {
        $tags = [
            ['tag' => 'direct', 'label' => '直连（内置）'],
            ['tag' => 'blocked', 'label' => '拦截（内置）'],
        ];
        $seen = ['direct' => true, 'blocked' => true, 'api' => true];

        foreach (XrayOutbound::enabledFor($node) as $outbound) {
            if (isset($seen[$outbound->tag])) {
                continue;
            }
            $seen[$outbound->tag] = true;
            $tags[] = [
                'tag' => $outbound->tag,
                // warp 出站固定显示 WARP（remark 可能为空的 wireguard 会认不出）
                'label' => $outbound->tag === 'warp' ? 'WARP' : ($outbound->remark ?: $outbound->protocol),
                // 中转出站标记：列表/下拉要显示「节点名」而非裸 tag（rl-* 看不出是谁的服务器）
                'relay' => $outbound->relay_target_node_id !== null,
            ];
        }

        // 订阅出站（服务器池）也是合法路由目标
        foreach (XrayOutboundSubscription::activeTags($node) as $tag) {
            if (isset($seen[$tag])) {
                continue;
            }
            $seen[$tag] = true;
            $tags[] = ['tag' => $tag, 'label' => '订阅'];
        }

        return $tags;
    }

    /** 出站 tag 纯字符串列表（balancer selector 匹配的候选）。 */
    private function outboundTagStrings(Node $node): array
    {
        return array_column($this->outboundTags($node), 'tag');
    }

    /**
     * 来源入站候选：本节点启用入站的对象数组 {value,tag,port,protocol,label}。
     * 前端显示「端口 · 协议 · tag」；value=tag（渲染层 inboundTag 用 xray tag 字符串）。
     */
    private function inboundCandidates(Node $node): array
    {
        return XrayInbound::enabledFor($node)->map(function (XrayInbound $in) {
            $label = $in->port . '-' . $in->tag; // 端口-端口名（本机入站不带节点名）

            return [
                'value' => $in->tag,
                'tag' => $in->tag,
                'port' => (int) $in->port,
                'protocol' => $in->protocol,
                'label' => $label,
            ];
        })->values()->all();
    }

    /** 本节点已启用 balancer 的 tag 列表。 */
    private function balancerTagStrings(Node $node): array
    {
        return XrayBalancer::enabledFor($node)->pluck('tag')->all();
    }

    /**
     * 规则目标候选：出站（内置 + 自定义）+ 已启用 balancer。
     * tag 全局唯一 → 前端下拉直接以 tag 为值，渲染层按在册情况判定 outboundTag / balancerTag。
     */
    private function targetTags(Node $node): array
    {
        $tags = $this->outboundTags($node);
        foreach (XrayBalancer::enabledFor($node) as $b) {
            $tags[] = ['tag' => $b->tag, 'label' => '负载均衡：' . ($b->remark ?: $b->strategy)];
        }

        return $tags;
    }

    private function validatePayload(Request $request, Node $node): array
    {
        $data = $request->validate([
            'domains' => ['nullable', 'array', 'max:200'],
            'domains.*' => ['string', 'max:512'],
            'ips' => ['nullable', 'array', 'max:200'],
            'ips.*' => ['string', 'max:512'],
            'port' => ['nullable', 'string', 'max:64'],
            'network' => ['nullable', 'in:tcp,udp'],
            'protocol' => ['nullable', 'in:http,tls,bittorrent'],
            'inbound_tag' => ['nullable', 'string', 'max:64'],
            'outbound_tag' => ['required', 'string', 'max:64'],
            'remark' => ['nullable', 'string', 'max:128'],
            'enabled' => ['nullable', 'boolean'],
            'sort' => ['nullable', 'integer', 'min:0', 'max:65535'],
        ]);

        // 至少一个匹配条件，否则会命中全部流量（渲染层也会跳过，这里直接拒绝）
        if (empty(array_filter($data['domains'] ?? [])) && empty(array_filter($data['ips'] ?? []))
            && empty($data['port']) && empty($data['network']) && empty($data['protocol']) && empty($data['inbound_tag'])) {
            throw ValidationException::withMessages(['domains' => '至少需要一个匹配条件（域名/IP/端口/网络/协议/入站）']);
        }

        // 目标 tag 必须存在（内置出站 / 自定义出站（含中转出站）/ 已启用 balancer）
        $target = (string) $data['outbound_tag'];
        $valid = array_column($this->targetTags($node), 'tag');
        if (! in_array($target, $valid, true)) {
            throw ValidationException::withMessages(['outbound_tag' => '目标出站不存在']);
        }

        return $data;
    }

    /**
     * balancer 载荷校验。
     *
     * - tag 同节点唯一（更新时排除自身）
     * - selector 至少一条，且每条都前缀匹配到「存在的出站」（xray selector 为前缀匹配）
     * - fallback_tag 若填，必须是存在的出站（balancer 不作为回退目标，避免环）
     */
    private function validateBalancer(Request $request, Node $node, ?XrayBalancer $self = null): array
    {
        $data = $request->validate([
            'tag' => ['required', 'string', 'max:64', 'regex:/^[A-Za-z0-9._-]+$/'],
            'selectors' => ['required', 'array', 'min:1', 'max:50'],
            'selectors.*' => ['string', 'max:64'],
            'strategy' => ['required', 'string', 'in:' . implode(',', XrayBalancer::STRATEGIES)],
            'fallback_tag' => ['nullable', 'string', 'max:64'],
            'remark' => ['nullable', 'string', 'max:128'],
            'enabled' => ['nullable', 'boolean'],
            'sort' => ['nullable', 'integer', 'min:0', 'max:65535'],
        ]);

        $tag = trim($data['tag']);
        $dupQuery = XrayBalancer::where('node_id', $node->id)->where('tag', $tag);
        if ($self !== null) {
            $dupQuery->where('id', '!=', $self->id);
        }
        if ($dupQuery->exists()) {
            throw ValidationException::withMessages(['tag' => '该负载均衡 tag 已存在']);
        }

        // tag 不能与出站 tag 撞名（渲染层 outboundTag / balancerTag 靠唯一 tag 区分）
        if (in_array($tag, $this->outboundTagStrings($node), true)) {
            throw ValidationException::withMessages(['tag' => '该 tag 与已有出站 tag 冲突']);
        }

        $outboundTags = $this->outboundTagStrings($node);
        $selectors = array_values(array_filter(array_map('trim', $data['selectors']), fn ($s) => $s !== ''));
        if ($selectors === []) {
            throw ValidationException::withMessages(['selectors' => '至少需要一个 selector']);
        }
        foreach ($selectors as $selector) {
            $matched = array_filter($outboundTags, fn ($t) => str_starts_with($t, $selector));
            if ($matched === []) {
                throw ValidationException::withMessages(['selectors' => "selector「{$selector}」没有匹配到任何出站"]);
            }
        }

        $fallback = trim((string) ($data['fallback_tag'] ?? ''));
        if ($fallback !== '' && ! in_array($fallback, $outboundTags, true)) {
            throw ValidationException::withMessages(['fallback_tag' => '回退出站不存在']);
        }

        $data['tag'] = $tag;
        $data['selectors'] = $selectors;
        $data['fallback_tag'] = $fallback !== '' ? $fallback : null;

        return $data;
    }

    private function fillBalancer(XrayBalancer $balancer, array $data): void
    {
        $balancer->tag = (string) $data['tag'];
        $balancer->selectors = $data['selectors'];
        $balancer->strategy = (string) $data['strategy'];
        $balancer->fallback_tag = $data['fallback_tag'];
        $balancer->remark = (string) ($data['remark'] ?? '') !== '' ? (string) $data['remark'] : null;
        if (array_key_exists('enabled', $data)) {
            $balancer->enabled = (bool) $data['enabled'];
        }
        if (array_key_exists('sort', $data)) {
            $balancer->sort = (int) $data['sort'];
        }
    }

    private function presentBalancer(XrayBalancer $balancer): array
    {
        return [
            'id' => $balancer->id,
            'tag' => $balancer->tag,
            'selectors' => array_values((array) ($balancer->selectors ?? [])),
            'strategy' => $balancer->strategy,
            'fallback_tag' => $balancer->fallback_tag,
            'remark' => $balancer->remark,
            'enabled' => $balancer->enabled,
            'sort' => $balancer->sort,
        ];
    }

    private function fillFromPayload(XrayRoutingRule $rule, array $data): void
    {
        $rule->domains = array_values(array_filter(array_map('trim', $data['domains'] ?? []), fn ($v) => $v !== '')) ?: null;
        $rule->ips = array_values(array_filter(array_map('trim', $data['ips'] ?? []), fn ($v) => $v !== '')) ?: null;
        $rule->port = $data['port'] ?? null;
        $rule->network = $data['network'] ?? null;
        $rule->protocol = $data['protocol'] ?? null;
        $rule->inbound_tag = $data['inbound_tag'] ?? null;
        $rule->outbound_tag = (string) $data['outbound_tag'];
        $rule->remark = (string) ($data['remark'] ?? '') !== '' ? (string) $data['remark'] : null;
        if (array_key_exists('enabled', $data)) {
            $rule->enabled = (bool) $data['enabled'];
        }
        if (array_key_exists('sort', $data)) {
            $rule->sort = (int) $data['sort'];
        }
    }

    /**
     * 域名匹配模拟。
     * @return bool|null true=命中；false=不命中；null=依赖 geo 数据无法精确判定
     */
    private function matchDomain(string $pattern, string $domain): ?bool
    {
        $domain = strtolower($domain);
        $pattern = trim($pattern);
        if ($pattern === '') {
            return false;
        }

        if (str_starts_with($pattern, 'geosite:')) {
            return null; // 需要内核 geosite 数据
        }
        if (str_starts_with($pattern, 'full:')) {
            return $domain === strtolower(substr($pattern, 5));
        }
        if (str_starts_with($pattern, 'domain:')) {
            $d = strtolower(substr($pattern, 7));

            return $domain === $d || str_ends_with($domain, '.' . $d);
        }
        if (str_starts_with($pattern, 'keyword:')) {
            return str_contains($domain, strtolower(substr($pattern, 8)));
        }
        if (str_starts_with($pattern, 'regexp:')) {
            return @preg_match('/' . str_replace('/', '\/', substr($pattern, 7)) . '/i', $domain) === 1;
        }
        if (str_starts_with($pattern, 'ext:')) {
            return str_ends_with($domain, '.' . strtolower(substr($pattern, 4)));
        }

        // 裸域名等价 domain:
        $d = strtolower($pattern);

        return $domain === $d || str_ends_with($domain, '.' . $d);
    }

    /**
     * IP 匹配模拟。
     * @return bool|null true=命中；false=不命中；null=依赖 geo 数据无法精确判定
     */
    private function matchIp(string $pattern, string $ip): ?bool
    {
        $pattern = trim($pattern);
        if ($pattern === '') {
            return false;
        }

        if (str_starts_with($pattern, 'geoip:')) {
            return null; // 需要内核 geoip 数据
        }

        if (! str_contains($pattern, '/')) {
            return $ip === $pattern;
        }

        // CIDR 判断
        [$subnet, $bits] = explode('/', $pattern, 2);
        $bits = (int) $bits;
        $ipBin = @inet_pton($ip);
        $subnetBin = @inet_pton($subnet);
        if ($ipBin === false || $subnetBin === false || strlen($ipBin) !== strlen($subnetBin)) {
            return false;
        }

        $fullBytes = intdiv($bits, 8);
        $remBits = $bits % 8;

        if (substr($ipBin, 0, $fullBytes) !== substr($subnetBin, 0, $fullBytes)) {
            return false;
        }
        if ($remBits === 0) {
            return true;
        }

        $mask = ~((1 << (8 - $remBits)) - 1) & 0xFF;

        return (ord($ipBin[$fullBytes]) & $mask) === (ord($subnetBin[$fullBytes]) & $mask);
    }

    private function present(XrayRoutingRule $rule): array
    {
        return [
            'id' => $rule->id,
            'domains' => array_values((array) ($rule->domains ?? [])),
            'ips' => array_values((array) ($rule->ips ?? [])),
            'port' => $rule->port,
            'network' => $rule->network,
            'protocol' => $rule->protocol,
            'inbound_tag' => $rule->inbound_tag,
            'outbound_tag' => $rule->outbound_tag,
            'remark' => $rule->remark,
            'enabled' => $rule->enabled,
            'sort' => $rule->sort,
        ];
    }
}
