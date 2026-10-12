<?php

namespace App\Drivers\Xray;

use App\Models\Node;
use App\Models\XrayInbound;
use App\Models\XrayOutbound;
use App\Models\XrayRelayClient;
use App\Models\XrayRoutingRule;
use App\Services\ThreeXUiClientFactory;
use Illuminate\Support\Str;
use Illuminate\Validation\ValidationException;

/**
 * xray 中转自动化：出站页以「其它机器」为目标新建出站时，保存即自动：
 *
 *   1) 在**源节点**建一条中转出站（xray_outbounds，tag 前缀 rl-{目标nodeId}-{目标port}）——
 *      协议取目标入站协议（vless/vmess/trojan），凭据与传输/reality 公开值取目标入站
 *      stream_settings（reality 用目标入站的 publicKey/serverNames/shortIds，与订阅链接同源）。
 *      ss 目标不支持（面板自建 ss 入站为单凭据模式、不便自动加客户端）。
 *
 *   2) 在**目标节点**建对应凭据：
 *      - xray 目标 → xray_relay_clients 落库，渲染时经 XrayConfigService::renderInbound
 *        注入该入站 clients（照 clientsFor 口径）
 *      - 3x-ui 目标 → ThreeXUiClient addClient + attach（inbound_id 取 node_inbounds）
 *
 * 删除回收（见 deleteRelayOutbound / cleanupRelayClient）：删除中转出站或删除引用它的
 * 最后一条路由规则时，回收目标节点上的承接凭据，不留垃圾 client。
 *
 * 幂等：同一「源节点 × 目标入站」重复保存复用已有出站/凭据，不产生重复。
 */
class XrayRelayService
{
    /** 中转出站 tag 前缀（区分手工出站，便于识别/回收）。 */
    public const TAG_PREFIX = 'rl-';

    public function __construct(
        private readonly ThreeXUiClientFactory $factory,
    ) {}

    /**
     * 为「源节点规则 → 目标入站」确保中转出站与目标凭据就绪，返回源节点上的中转出站 tag。
     *
     * @param  int  $targetNodeId    目标节点
     * @param  int  $targetInboundId 目标入站（xray 目标 = xray_inbounds.id；3x 目标 = node_inbounds.inbound_id）
     */
    public function ensureRelay(Node $source, int $targetNodeId, int $targetInboundId): string
    {
        $target = Node::find($targetNodeId);
        if ($target === null) {
            throw ValidationException::withMessages(['outbound_tag' => '目标节点不存在']);
        }

        if ($target->isXray()) {
            $inbound = XrayInbound::where('node_id', $target->id)->find($targetInboundId);
            if ($inbound === null) {
                throw ValidationException::withMessages(['outbound_tag' => '目标入站不存在']);
            }
            if (! $inbound->enabled) {
                throw ValidationException::withMessages(['outbound_tag' => '目标入站未启用']);
            }
            if ($inbound->protocol === 'shadowsocks') {
                throw ValidationException::withMessages(['outbound_tag' => '目标入站协议（shadowsocks）暂不支持自动中转']);
            }

            $clientId = $this->ensureXrayRelayClient($source, $target, $inbound);

            return $this->ensureRelayOutbound(
                $source,
                $target,
                (int) $inbound->port,
                $inbound->id,
                $inbound->protocol,
                $this->outboundSettings(
                    $inbound->protocol,
                    $clientId,
                    (string) $target->host,
                    (int) $inbound->port,
                    $this->flowFor($inbound),
                ),
                $this->outboundStream($inbound, (string) $target->host),
                $target->name . '-' . $inbound->port . '-' . $inbound->tag,
            );
        }

        // ── 3x-ui 目标 ──
        $row = $target->inbounds()->where('inbound_id', $targetInboundId)->first();
        if ($row === null) {
            throw ValidationException::withMessages(['outbound_tag' => '目标节点的该入站不存在']);
        }
        $protocol = (string) $row->protocol;
        if ($protocol === 'shadowsocks') {
            throw ValidationException::withMessages(['outbound_tag' => '目标入站协议（shadowsocks）暂不支持自动中转']);
        }

        [$port, $tag, $stream] = $this->threeXInboundMeta($target, $targetInboundId);
        $port = $port ?: 443;
        $clientId = $this->ensureThreeXClient($source, $target, $targetInboundId, $protocol);

        // remark 用三段式「节点名-端口-端口名（tag）」，列表/下拉直接可读
        $remark = implode('-', array_values(array_filter(
            [$target->name, (string) $port, $tag],
            fn ($v) => $v !== '',
        )));

        return $this->ensureRelayOutbound(
            $source,
            $target,
            $port,
            $targetInboundId,
            $protocol,
            $this->outboundSettings($protocol, $clientId, (string) $target->host, $port),
            $this->outboundStreamFromThreeX($stream, (string) $target->host),
            $remark,
        );
    }

    /**
     * 删除一条路由规则后回收其可能引用的中转出站。
     *
     * 若该规则引用的出站是自动中转出站（relay_target_node_id 非空）且**没有其它规则再引用它**
     * → 删除该出站并清理对方节点上的承接凭据（见 deleteRelayOutbound）；仍有其它规则引用时保留。
     *
     * 调用方须在规则删除**之后**调用（此时引用计数已不含该规则）。
     */
    public function cleanupOutboundForRule(Node $node, XrayRoutingRule $rule): void
    {
        $outbound = XrayOutbound::where('node_id', $node->id)
            ->where('tag', $rule->outbound_tag)
            ->whereNotNull('relay_target_node_id')
            ->first();

        if ($outbound === null) {
            return; // 非中转出站 / 已不存在
        }

        $stillReferenced = $node->xrayRoutingRules()
            ->where('outbound_tag', $outbound->tag)
            ->exists();

        if ($stillReferenced) {
            return; // 还有别的规则引用它 → 保留出站与凭据
        }

        $this->deleteRelayOutbound($outbound);
    }

    /**
     * 删除一条中转出站：清理对方节点上的承接凭据 + 删本地出站记录。
     *
     * - xray 目标 → 删 xray_relay_clients 行
     * - 3x-ui 目标 → 调面板 clients/del 删 client（email 口径与 ensureThreeXClient 一致）
     *
     * 对方清理失败不阻断本地删除（best-effort）：面板不可达时仍要能让出站删掉，
     * 残留的孤儿凭据可在对方节点手工清理。
     */
    public function deleteRelayOutbound(XrayOutbound $outbound): void
    {
        $this->cleanupRelayClient(
            (int) $outbound->node_id,
            $outbound->relay_target_node_id !== null ? (int) $outbound->relay_target_node_id : null,
            $outbound->relay_target_inbound_id !== null ? (int) $outbound->relay_target_inbound_id : null,
        );

        $outbound->delete();
    }

    /**
     * 清理「源节点 × 目标入站」在目标节点上的承接凭据（best-effort）。
     *
     * @param int|null $targetNodeId    对方节点（null → 非中转出站，跳过）
     * @param int|null $targetInboundId 对方入站
     */
    public function cleanupRelayClient(?int $sourceNodeId, ?int $targetNodeId, ?int $targetInboundId): void
    {
        if ($targetNodeId === null || $targetInboundId === null) {
            return;
        }

        $target = Node::find($targetNodeId);
        if ($target === null) {
            return; // 对方节点已删，无处可清理
        }

        if ($target->isXray()) {
            // xray 目标：仅删本「源节点」在目标入站上的那条凭据，不影响该入站其它用户。
            if ($sourceNodeId !== null) {
                XrayRelayClient::where('node_id', $target->id)
                    ->where('inbound_id', $targetInboundId)
                    ->where('source_node_id', $sourceNodeId)
                    ->delete();
            }

            return;
        }

        // 3x-ui 目标：email 口径与 ensureThreeXClient 一致（relay_{源}_{目标入站}）
        if ($sourceNodeId === null) {
            return;
        }
        $email = substr(sprintf('relay_%d_%d', $sourceNodeId, $targetInboundId), 0, 128);
        try {
            $this->factory->forNode($target)->deleteClient($email);
        } catch (\Throwable) {
            // 面板不可达 / client 已不存在 → 不阻断本地删除
        }
    }

    /**
     * 源节点中转出站「已存在则刷新，否则落库」。返回出站 tag。
     *
     * 幂等键 =（源节点, relay_target_node_id, relay_target_inbound_id）——同一对
     * 「源节点 × 目标入站」恒为一条记录，重复保存不产生重复。
     *
     * 命中已有行时**仍回写 settings/stream/remark**：目标入站的凭据、
     * reality/xhttp 参数可能在首次保存后才补齐或被改动，沿用旧值会让中转一直握
     * 着不含 uuid 的出站（实测因此打不出目标出口 IP）。tag 保持不变，路由规则引用不受影响。
     */
    private function ensureRelayOutbound(
        Node $source,
        Node $target,
        int $port,
        int $targetInboundId,
        string $protocol,
        array $settings,
        array $stream,
        string $remark,
    ): string {
        $existing = XrayOutbound::where('node_id', $source->id)
            ->where('relay_target_node_id', $target->id)
            ->where('relay_target_inbound_id', $targetInboundId)
            ->first();

        $outbound = $existing ?? new XrayOutbound(['node_id' => $source->id]);
        if ($existing === null) {
            $outbound->tag = $this->uniqueTag($source, $target->id, $port ?: $targetInboundId);
        }
        $outbound->protocol = $protocol;
        $outbound->relay_target_node_id = $target->id;
        $outbound->relay_target_inbound_id = $targetInboundId;
        $outbound->settings = $settings;
        $outbound->stream_settings = $stream;
        $outbound->remark = $remark;
        $outbound->enabled = true;
        $outbound->save();

        return $outbound->tag;
    }

    /**
     * xray 目标：确保「目标入站」上有一条 relay 凭据（uuid/password）。返回凭据。
     * 同一「源节点 × 目标入站」复用已有凭据。
     */
    private function ensureXrayRelayClient(Node $source, Node $target, XrayInbound $inbound): string
    {
        $existing = XrayRelayClient::where('node_id', $target->id)
            ->where('inbound_id', $inbound->id)
            ->where('source_node_id', $source->id)
            ->first();
        if ($existing !== null) {
            return (string) $existing->client_id;
        }

        $clientId = $inbound->protocol === 'trojan' ? Str::random(32) : (string) Str::uuid();

        XrayRelayClient::create([
            'node_id' => $target->id,
            'inbound_id' => $inbound->id,
            'email' => substr(sprintf('relay_%d_%d_%d', $source->id, $target->id, $inbound->id), 0, 128),
            'client_id' => $clientId,
            'source_node_id' => $source->id,
            'source_inbound_id' => 0,
        ]);

        return $clientId;
    }

    /**
     * 3x-ui 目标：ensure client（email 幂等）→ attach 到目标入站。返回 Xray 用的 client id（uuid）。
     *
     * email 口径：同「源节点 × 目标入站」稳定不变，重复保存复用同一 client。
     * 3x-ui addClient 服务端生成 uuid；attach 后回查一次取其 **uuid**。
     *
     * 取值口径与 UserAdminService::provisionThreeXClient 一致：3x-ui v3.7.0 的 clients/get
     * 回执里 `id` 是 clients 表自增行号、`uuid` 才是 Xray 的 client id。中转出站 vnext.users.id
     * 必须写 uuid，写行号会让对端把该 client 的 uuid 覆盖成数字而断连（实测表现为出站无凭据可用）。
     */
    private function ensureThreeXClient(Node $source, Node $target, int $targetInboundId, string $protocol = 'vless'): string
    {
        $email = substr(sprintf('relay_%d_%d', $source->id, $targetInboundId), 0, 128);
        $client = $this->factory->forNode($target);

        // 幂等：已存在则只补挂载
        if ($client->getClient($email) === null) {
            $client->addClient([
                'email' => $email,
                'enable' => true,
                'totalGB' => 0,
                'expiryTime' => 0,
                'limitIp' => 0,
                'flow' => '',
            ], [$targetInboundId]);
        }

        // attach 幂等（已挂再挂无副作用）
        $client->attachClient($email, [$targetInboundId]);

        $fresh = $client->getClient($email);
        if ($fresh === null) {
            return '';
        }

        // trojan 凭据在 password，vless/vmess 在 uuid。都取不到时返回空：
        // 宁可上层建成「无凭据出站」，也不能回退写 id —— 那是 clients 表自增行号，
        // 写进 vnext.users.id 会让对端把该 client 的真 uuid 覆盖成数字而断连。
        return (string) ($protocol === 'trojan'
            ? ($fresh['password'] ?? '')
            : ($fresh['uuid'] ?? ''));
    }

    /**
     * 3x-ui 入站元数据（listInbounds 里取）：端口 + tag + 已解析的 streamSettings。
     *
     * streamSettings 在 3x-ui 的 list 响应里是 JSON **字符串**（历史版本为对象），此处
     * 统一解析成数组供 outboundStreamFromThreeX 使用。
     *
     * @return array{0:int,1:string,2:array} [port, tag, streamSettings]；拿不到返回 [0, '', []]
     */
    private function threeXInboundMeta(Node $target, int $inboundId): array
    {
        try {
            foreach ($this->factory->forNode($target)->listInbounds() as $inbound) {
                if ((int) ($inbound['id'] ?? 0) === $inboundId) {
                    return [
                        (int) ($inbound['port'] ?? 0),
                        (string) ($inbound['tag'] ?? ''),
                        $this->normalizeStream($inbound['streamSettings'] ?? []),
                    ];
                }
            }
        } catch (\Throwable) {
            // 拉不到面板入站清单不阻断中转（端口/tag/stream 缺失只影响展示/传输）
        }

        return [0, '', []];
    }

    // ── 出站结构与渲染口径 ─────────────────────────────

    /** 中转出站 settings（凭据 + 目标地址/端口 + vless flow）。 */
    private function outboundSettings(string $protocol, string $clientId, string $address, int $port, string $flow = ''): array
    {
        $port = $port ?: 443;

        return match ($protocol) {
            'vmess' => [
                'vnext' => [[
                    'address' => $address,
                    'port' => $port,
                    'users' => [['id' => $clientId, 'alterId' => 0, 'security' => 'auto']],
                ]],
            ],
            'trojan' => [
                'servers' => [[
                    'address' => $address,
                    'port' => $port,
                    'password' => $clientId,
                ]],
            ],
            default => [ // vless
                'vnext' => [[
                    'address' => $address,
                    'port' => $port,
                    'users' => [array_filter([
                        'id' => $clientId,
                        'encryption' => 'none',
                        'flow' => $flow,
                    ], fn ($v) => $v !== '')],
                ]],
            ],
        };
    }

    /**
     * 目标入站若为 vless + tcp + reality/tls（vision 流控适用），中转出站用户须带 flow，
     * 与渲染层 flowApplies / 订阅链接同源；否则对端按 vision 处理而客户端没带 → 握手失败。
     */
    private function flowFor(XrayInbound $inbound): string
    {
        $stream = $inbound->stream_settings ?? [];
        if ($inbound->protocol === 'vless'
            && ($stream['network'] ?? 'tcp') === 'tcp'
            && in_array($stream['security'] ?? 'none', ['reality', 'tls'], true)) {
            return XrayConfigService::FLOW;
        }

        return '';
    }

    /**
     * 中转出站 streamSettings：传输层取目标入站；reality/tls 用目标入站的**公开**参数
     * （publicKey / serverNames / shortIds），与订阅链接生成同源（不涉私钥）。
     */
    private function outboundStream(XrayInbound $inbound, string $host): array
    {
        $stream = $inbound->stream_settings ?? [];
        $security = (string) ($stream['security'] ?? 'none');
        $network = (string) ($stream['network'] ?? 'tcp');

        $out = ['network' => $network, 'security' => $security];

        if ($security === 'reality') {
            $reality = $stream['realitySettings'] ?? [];
            $client = is_array($reality['settings'] ?? null) ? $reality['settings'] : [];
            $setting = [
                'serverName' => (string) ($reality['serverNames'][0] ?? XrayConfigService::DEFAULT_SNI),
                'publicKey' => (string) ($reality['publicKey'] ?? ''),
                'fingerprint' => (string) ($client['fingerprint'] ?? 'chrome') ?: 'chrome',
            ];
            $shortId = (string) ($reality['shortIds'][0] ?? '');
            if ($shortId !== '') {
                $setting['shortId'] = $shortId;
            }
            if (! empty($client['spiderX'])) {
                $setting['spiderX'] = (string) $client['spiderX'];
            }
            $out['realitySettings'] = $setting;
        } elseif ($security === 'tls') {
            $out['tlsSettings'] = array_filter([
                'serverName' => (string) ($stream['tlsSettings']['serverName'] ?? $host),
            ], fn ($v) => $v !== '');
        }

        if ($network === 'ws') {
            $ws = $stream['wsSettings'] ?? [];
            $out['wsSettings'] = array_filter([
                'path' => (string) ($ws['path'] ?? '/'),
                'headers' => ! empty($ws['host']) ? ['Host' => (string) $ws['host']] : null,
            ], fn ($v) => $v !== null);
        }

        return $out;
    }

    /**
     * 3x-ui 目标入站的 streamSettings（面板格式）→ 中转出站客户端 streamSettings。
     *
     * 与 xray 目标的 outboundStream 关键差异（已按实测线上数据核对）：
     *   - reality 的 **publicKey 在 realitySettings 顶层**（不在客户端段 settings 里），
     *     客户端段 settings 只放 fingerprint / spiderX / mldsa65Verify；而 xray 目标的
     *     publicKey 恰在 settings 里。故 publicKey 必须顶层优先、兼容 settings 回退。
     *   - 网络类型在 `network`（auto→tcp）；xhttp 参数在 `xhttpSettings`（path/host/mode）。
     *   - serverNames/shortIds 在顶层；取值口径与订阅链接同源（各取第 0 个，不涉私钥）。
     *   - 目标入站用 xhttp 时 vless 出站用户**不带 flow**（vision 仅限 tcp），
     *     否则对端按 vision 处理而客户端没带 → 握手失败。
     */
    private function outboundStreamFromThreeX(array $stream, string $host): array
    {
        if ($stream === []) {
            return [];
        }

        $security = (string) ($stream['security'] ?? 'none');
        $network = (string) ($stream['network'] ?? 'tcp');
        if ($network === '' || $network === 'auto') {
            $network = 'tcp';
        }

        $out = ['network' => $network, 'security' => $security];

        if ($security === 'reality') {
            $reality = is_array($stream['realitySettings'] ?? null) ? $stream['realitySettings'] : [];
            $client = is_array($reality['settings'] ?? null) ? $reality['settings'] : [];
            $setting = [
                'serverName' => (string) ($reality['serverNames'][0] ?? XrayConfigService::DEFAULT_SNI),
                // publicKey 顶层优先（3x-ui），settings 回退（xray 写法）
                'publicKey' => (string) ($reality['publicKey'] ?? ($client['publicKey'] ?? '')),
                'fingerprint' => (string) ($client['fingerprint'] ?? 'chrome') ?: 'chrome',
            ];
            $shortId = (string) ($reality['shortIds'][0] ?? '');
            if ($shortId !== '') {
                $setting['shortId'] = $shortId;
            }
            if (! empty($client['spiderX'])) {
                $setting['spiderX'] = (string) $client['spiderX'];
            }
            $out['realitySettings'] = $setting;
        } elseif ($security === 'tls') {
            $tls = $stream['tlsSettings'] ?? [];
            $out['tlsSettings'] = array_filter([
                'serverName' => (string) ($tls['serverName'] ?? $host),
            ], fn ($v) => $v !== '');
        }

        if ($network === 'ws') {
            $ws = $stream['wsSettings'] ?? [];
            $out['wsSettings'] = array_filter([
                'path' => (string) ($ws['path'] ?? '/'),
                'headers' => ! empty($ws['host']) ? ['Host' => (string) $ws['host']] : null,
            ], fn ($v) => $v !== null);
        } elseif ($network === 'xhttp') {
            $xhttp = $stream['xhttpSettings'] ?? [];
            $out['xhttpSettings'] = array_filter([
                'path' => (string) ($xhttp['path'] ?? '/'),
                'host' => (string) ($xhttp['host'] ?? ''),
                'mode' => (string) ($xhttp['mode'] ?? 'auto') ?: 'auto',
            ], fn ($v) => $v !== '');
        } elseif ($network === 'grpc') {
            $grpc = $stream['grpcSettings'] ?? [];
            $out['grpcSettings'] = array_filter([
                'serviceName' => (string) ($grpc['serviceName'] ?? ''),
                'authority' => (string) ($grpc['authority'] ?? ''),
            ], fn ($v) => $v !== '');
        }

        return $out;
    }

    // ── 工具 ─────────────────────────────────────────────

    /**
     * 归一化「传输设置」为数组：3x-ui 的 listInbounds 把 streamSettings 序列化成
     * JSON **字符串**，个别版本返回对象。读取目标入站传输参数前统一走这里。
     *
     * @return array 解析失败 / 空值 → []（调用侧按「无传输配置」处理）
     */
    private function normalizeStream(mixed $stream): array
    {
        if (is_string($stream)) {
            if ($stream === '') {
                return [];
            }
            $decoded = json_decode($stream, true);

            return is_array($decoded) ? $decoded : [];
        }

        return is_array($stream) ? $stream : [];
    }

    /** 源节点内唯一的 tag：rl-{目标节点}-{port}，冲突自动加序号。 */
    private function uniqueTag(Node $source, int $targetNodeId, int $port): string
    {
        $base = self::TAG_PREFIX . $targetNodeId . '-' . ($port ?: 'x');
        $base = preg_replace('/[^A-Za-z0-9_.-]/', '', $base) ?: self::TAG_PREFIX . 'relay';

        $existing = $source->xrayOutbounds()->pluck('tag')->all();
        $tag = $base;
        $i = 2;
        while (in_array($tag, $existing, true)) {
            $tag = $base . '-' . $i;
            $i++;
        }

        return $tag;
    }
}
