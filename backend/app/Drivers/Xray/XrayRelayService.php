<?php

namespace App\Drivers\Xray;

use App\Models\Node;
use App\Models\XrayInbound;
use App\Models\XrayOutbound;
use App\Models\XrayRelayClient;
use App\Services\ThreeXUiClientFactory;
use Illuminate\Support\Str;
use Illuminate\Validation\ValidationException;

/**
 * xray 中转自动化：路由目标选「其它机器的入站」时，保存规则即自动：
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

        [$port, $tag] = $this->threeXInboundMeta($target, $targetInboundId);
        $port = $port ?: 443;
        $clientId = $this->ensureThreeXClient($source, $target, $targetInboundId);

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
            [],
            $remark,
        );
    }

    /**
     * 源节点中转出站「已存在则复用，否则落库」。返回出站 tag。
     *
     * 幂等键 =（源节点, relay_target_node_id, relay_target_inbound_id）。
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

        if ($existing !== null && $existing->protocol === $protocol) {
            return $existing->tag;
        }

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
     * 3x-ui 目标：ensure client（email 幂等）→ attach 到目标入站。返回 client 的 uuid。
     *
     * email 口径：同「源节点 × 目标入站」稳定不变，重复保存复用同一 client。
     * 3x-ui addClient 服务端生成 uuid；attach 后回查一次取 id。
     */
    private function ensureThreeXClient(Node $source, Node $target, int $targetInboundId): string
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

        return (string) ($fresh['id'] ?? $fresh['uuid'] ?? '');
    }

    /**
     * 3x-ui 入站端口 + tag（listInbounds 里取）。
     *
     * @return array{0:int,1:string} [port, tag]；拿不到返回 [0, '']（调用侧回落）
     */
    private function threeXInboundMeta(Node $target, int $inboundId): array
    {
        try {
            foreach ($this->factory->forNode($target)->listInbounds() as $inbound) {
                if ((int) ($inbound['id'] ?? 0) === $inboundId) {
                    return [(int) ($inbound['port'] ?? 0), (string) ($inbound['tag'] ?? '')];
                }
            }
        } catch (\Throwable) {
            // 拉不到面板入站清单不阻断中转（端口/tag 缺失只影响展示）
        }

        return [0, ''];
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

    // ── 工具 ─────────────────────────────────────────────

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
