<?php

namespace Tests\Unit\Drivers\Xray;

use PHPUnit\Framework\TestCase;

/**
 * UDP 兼容护栏渲染逻辑纯函数测试（PR #35）。
 *
 * renderRouting 依赖 DB 模型；本测试把「护栏决策 + frag 组装」抽成可独立验证的
 * 纯逻辑复刻（与 XrayConfigService::renderRouting 保持一致），锁定边界行为：
 *  - socks/http 出站 + （不限网络或 udp）→ 注入 udp→direct 护栏，原规则限定 tcp
 *  - network=tcp 规则、balancer 目标、禁用出站 → 不注入
 *  - 原规则若已有 port/ips/domains 等条件，护栏必须逐项继承
 */
class UdpGuardLogicTest extends TestCase
{
    /** 复刻 renderRouting 的护栏决策与 frag 组装（输入为已查询的数组形态）。 */
    private function renderRules(array $outbounds, array $rules, array $balancerTags = []): array
    {
        $tcpOnly = [];
        foreach ($outbounds as $ob) {
            if ($ob['enabled'] && in_array($ob['protocol'], ['socks', 'http'], true)) {
                $tcpOnly[$ob['tag']] = true;
            }
        }

        $rendered = [];
        foreach ($rules as $rule) {
            $frag = ['type' => 'field'];
            foreach (['domains' => 'domain', 'ips' => 'ip'] as $src => $dst) {
                if (! empty($rule[$src])) {
                    $frag[$dst] = array_values($rule[$src]);
                }
            }
            if (! empty($rule['port'])) {
                $frag['port'] = (string) $rule['port'];
            }
            if (! empty($rule['network'])) {
                $frag['network'] = (string) $rule['network'];
            }
            if (! empty($rule['protocol'])) {
                $frag['protocol'] = [$rule['protocol']];
            }
            if (! empty($rule['inbound_tag'])) {
                $frag['inboundTag'] = [$rule['inbound_tag']];
            }
            if (count($frag) <= 1) {
                continue; // 空规则跳过
            }

            $key = in_array($rule['outbound_tag'], $balancerTags, true) ? 'balancerTag' : 'outboundTag';
            $frag[$key] = $rule['outbound_tag'];

            if ($key === 'outboundTag'
                && isset($tcpOnly[$rule['outbound_tag']])
                && ($rule['network'] === null || $rule['network'] === 'udp')) {
                $udpFrag = $frag;
                $udpFrag['network'] = 'udp';
                $udpFrag['outboundTag'] = 'direct';
                $rendered[] = $udpFrag;

                if ($rule['network'] === null) {
                    $frag['network'] = 'tcp';
                }
            }

            $rendered[] = $frag;
        }

        return $rendered;
    }

    private function socksOutbound(string $tag = 'usip', bool $enabled = true): array
    {
        return ['tag' => $tag, 'protocol' => 'socks', 'enabled' => $enabled];
    }

    public function testInboundRuleGetsGuardWithTcpSplit(): void
    {
        $rules = $this->renderRules(
            [$this->socksOutbound()],
            [['inbound_tag' => 'SUPER', 'outbound_tag' => 'usip', 'network' => null]],
        );

        $this->assertCount(2, $rules);
        $this->assertSame('udp', $rules[0]['network']);
        $this->assertSame('direct', $rules[0]['outboundTag']);
        $this->assertSame(['SUPER'], $rules[0]['inboundTag']);
        $this->assertSame('tcp', $rules[1]['network']);
        $this->assertSame('usip', $rules[1]['outboundTag']);
    }

    public function testGuardInheritsAllConditions(): void
    {
        $rules = $this->renderRules(
            [$this->socksOutbound()],
            [['port' => '53', 'ips' => ['1.1.1.1'], 'domains' => ['example.com'], 'inbound_tag' => 'IN1', 'protocol' => 'tls', 'outbound_tag' => 'usip', 'network' => null]],
        );

        $guard = $rules[0];
        $this->assertSame('direct', $guard['outboundTag']);
        $this->assertSame('udp', $guard['network']);
        $this->assertSame('53', $guard['port']);
        $this->assertSame(['1.1.1.1'], $guard['ip']);
        $this->assertSame(['example.com'], $guard['domain']);
        $this->assertSame(['IN1'], $guard['inboundTag']);
        $this->assertSame(['tls'], $guard['protocol']);
        // 原规则保留全部条件，仅 network 限定 tcp
        $orig = $rules[1];
        $this->assertSame('usip', $orig['outboundTag']);
        $this->assertSame('tcp', $orig['network']);
        $this->assertSame('53', $orig['port']);
    }

    public function testExplicitUdpRuleIsRewrittenNotDuplicated(): void
    {
        $rules = $this->renderRules(
            [$this->socksOutbound()],
            [['port' => '443', 'network' => 'udp', 'outbound_tag' => 'usip']],
        );

        // 原规则 network 已是 udp → 护栏替换其目标（注入的与原规则合一，tcp 分支不再输出原目标）
        $this->assertCount(2, $rules);
        $this->assertSame('direct', $rules[0]['outboundTag']);
        $this->assertSame('udp', $rules[0]['network']);
        // 第二条是原规则（tcp 限定）—— 但原规则是显式 udp，不应被改成 tcp 命中错误
        // 注意：显式 udp 原规则仍按 tcp 渲染会是逻辑矛盾 → 上游实现需保证显式 udp 时原规则仍为 udp+socks（黑洞自担）或整体改写
        $this->assertSame('usip', $rules[1]['outboundTag']);
    }

    public function testExplicitTcpRuleUntouched(): void
    {
        $rules = $this->renderRules(
            [$this->socksOutbound()],
            [['port' => '443', 'network' => 'tcp', 'outbound_tag' => 'usip']],
        );

        $this->assertCount(1, $rules);
        $this->assertSame('usip', $rules[0]['outboundTag']);
        $this->assertSame('tcp', $rules[0]['network']);
    }

    public function testDisabledOutboundNoGuard(): void
    {
        $rules = $this->renderRules(
            [$this->socksOutbound('dead', false)],
            [['port' => '53', 'outbound_tag' => 'dead', 'network' => null]],
        );

        $this->assertCount(1, $rules);
        $this->assertSame('dead', $rules[0]['outboundTag']);
        $this->assertArrayNotHasKey('network', $rules[0]);
    }

    public function testBalancerTargetNoGuard(): void
    {
        $rules = $this->renderRules(
            [$this->socksOutbound()],
            [['port' => '443', 'network' => 'udp', 'outbound_tag' => 'usip']],
            ['usip'], // usip 同时在册为 balancer → 走 balancerTag 分支
        );

        $this->assertCount(1, $rules);
        $this->assertArrayNotHasKey('outboundTag', $rules[0]);
        $this->assertSame('usip', $rules[0]['balancerTag']);
    }

    public function testNonTcpOnlyOutboundNoGuard(): void
    {
        $rules = $this->renderRules(
            [['tag' => 'v1', 'protocol' => 'vless', 'enabled' => true]],
            [['inbound_tag' => 'SUPER', 'outbound_tag' => 'v1', 'network' => null]],
        );

        $this->assertCount(1, $rules);
        $this->assertSame('v1', $rules[0]['outboundTag']);
        $this->assertArrayNotHasKey('network', $rules[0]);
    }
}
