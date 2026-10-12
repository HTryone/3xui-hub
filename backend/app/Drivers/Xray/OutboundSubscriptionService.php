<?php

namespace App\Drivers\Xray;

use App\Models\XrayOutboundSubscription;
use Illuminate\Support\Facades\Http;

/**
 * 出站订阅（服务器池）拉取/解析/标签稳定/缓存。
 *
 * 对齐 3x-ui internal/web/service/outbound_subscription.go：
 *  - 拉取：仅 http(s)，SSRF 防护（禁私网/保留/回环/CGNAT/组播 IP），重定向逐跳校验
 *  - 解析：复用 OutboundLinkParser（vless/vmess/trojan/ss/socks/http）
 *  - 标签稳定：为每条链接算 identity（去 remark），identity → tag 映射持久化；
 *    下次刷新同一 identity 复用旧 tag，路由/负载均衡引用不被打断
 *  - 内容变化才 bump 节点 config_version（免无谓重载）
 *
 * 失败保留上次成功缓存（fetched 不清空），只记 last_status/last_error。
 */
class OutboundSubscriptionService
{
    /** 拉取超时（秒）：订阅在境外，跨境 RTT 可能较高。 */
    private const FETCH_TIMEOUT = 30;
    private const CONNECT_TIMEOUT = 6;

    /** 单订阅解析出的出站上限，防异常响应撑爆。 */
    private const MAX_OUTBOUNDS = 2000;

    /** identity 用：这些 tag 不可占用。 */
    private const RESERVED_TAGS = ['direct', 'blocked', 'api'];

    /**
     * 测试缝：自定义主机名 → IP 解析器（callable(string $host): string[]）。
     * null = 走系统 DNS（默认）。注入它可让 SSRF 校验在无网络的测试里可验证。
     *
     * @var (callable(string): array<int,string>)|null
     */
    public static $hostResolver = null;

    // ── 公开 API ─────────────────────────────────────────

    /**
     * 校验并规范化订阅 URL：仅 http/https + 非内网。非法抛 InvalidArgumentException。
     */
    public function sanitizeUrl(string $url): string
    {
        $url = trim($url);
        if ($url === '') {
            throw new \InvalidArgumentException('订阅地址不能为空');
        }

        $parts = parse_url($url);
        if ($parts === false || empty($parts['host'])) {
            throw new \InvalidArgumentException('订阅地址格式不正确');
        }

        $scheme = strtolower((string) ($parts['scheme'] ?? ''));
        if (! in_array($scheme, ['http', 'https'], true)) {
            throw new \InvalidArgumentException('订阅地址仅支持 http/https');
        }

        $this->assertPublicHost((string) $parts['host'], $scheme);

        return $url;
    }

    /**
     * 刷新一条订阅：拉取 → 解析 → 标签稳定 → 缓存。成功返回出站数组；失败返回 null
     * （失败保留上次成功缓存，只记错）。
     *
     * @return array<int,array>|null
     */
    public function refresh(XrayOutboundSubscription $sub): ?array
    {
        return $this->fetchAndStore($sub);
    }

    /** 刷新所有「到期」的启用订阅（last_fetched_at + update_interval ≤ now）。返回成功条数。 */
    public function refreshDue(): int
    {
        $subs = XrayOutboundSubscription::where('enabled', true)->get();
        $now = now();
        $refreshed = 0;

        foreach ($subs as $sub) {
            $interval = max(60, (int) ($sub->update_interval ?: 600));
            $due = $sub->last_fetched_at === null
                || $sub->last_fetched_at->copy()->addSeconds($interval)->lessThanOrEqualTo($now);
            if (! $due) {
                continue;
            }
            if ($this->fetchAndStore($sub) !== null) {
                $refreshed++;
            }
        }

        return $refreshed;
    }

    // ── 拉取 + 解析 + 稳定 + 落库 ────────────────────────

    private function fetchAndStore(XrayOutboundSubscription $sub): ?array
    {
        try {
            $sub->url = $this->sanitizeUrl((string) $sub->url);
        } catch (\InvalidArgumentException $e) {
            $this->recordError($sub, $e->getMessage());

            return null;
        }

        try {
            $body = $this->fetchBody((string) $sub->url);
            $parsed = $this->parseBody($body);
            if (empty($parsed['outbounds'])) {
                throw new \RuntimeException('拉取成功但未解析到任何可用出站');
            }
        } catch (\Throwable $e) {
            $this->recordError($sub, $e->getMessage());

            return null;
        }

        $outbounds = $parsed['outbounds'];
        $identities = $parsed['identities'];

        // 上一次的 identity→tag 映射 + 位置→tag 兜底
        $prev = $sub->identities();
        $prevTagByIndex = [];
        foreach ($sub->outbounds() as $i => $o) {
            if (! empty($o['tag'])) {
                $prevTagByIndex[$i] = (string) $o['tag'];
            }
        }

        $assigned = self::assignStableTags(
            $outbounds,
            $identities,
            $prev,
            $prevTagByIndex,
            (int) $sub->id,
            (string) ($sub->tag_prefix ?? ''),
        );

        $newIdent = [];
        foreach ($outbounds as $i => $_) {
            $outbounds[$i]['tag'] = $assigned[$i];
            $id = $identities[$i] ?? '';
            if ($id !== '') {
                $newIdent[$id] = $assigned[$i];
            }
        }

        $json = (string) json_encode($outbounds, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE);
        $hash = sha1($json);
        $changed = $hash !== (string) $sub->content_hash;

        $sub->forceFill([
            'fetched' => $json,
            'link_identities' => (string) json_encode($newIdent, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE),
            'content_hash' => $hash,
            'node_count' => count($outbounds),
            'last_status' => 'success',
            'last_fetched_at' => now(),
            'last_error' => null,
        ])->save();

        // 内容变化才 bump 版本 → 节点重拉 config
        if ($changed) {
            $sub->node?->bumpXrayConfigVersion();
        }

        return $outbounds;
    }

    /** 拉取订阅正文（Http facade；重定向逐跳 SSRF 校验）。 */
    private function fetchBody(string $url): string
    {
        $response = Http::timeout(self::FETCH_TIMEOUT)
            ->connectTimeout(self::CONNECT_TIMEOUT)
            ->withHeaders([
                'User-Agent' => 'ControlHub-outbound-sub/1.0',
                'Accept' => '*/*',
            ])
            ->withOptions([
                'allow_redirects' => [
                    'max' => 5,
                    'on_redirect' => function ($request, $response, $uri): void {
                        $this->assertPublicHost((string) $uri->getHost(), (string) $uri->getScheme());
                    },
                ],
            ])
            ->get($url);

        if (! $response->successful()) {
            throw new \RuntimeException('HTTP ' . $response->status());
        }

        $body = trim((string) $response->body());
        if ($body === '') {
            throw new \RuntimeException('拉取内容为空');
        }

        return $body;
    }

    /**
     * 解析订阅正文（明文逐行 / 整段 base64）→ 出站数组 + identity 数组。
     * 坏行/不支持协议静默跳过（对齐 3x-ui）。
     *
     * @return array{outbounds: array<int,array>, identities: array<int,string>}
     */
    public function parseBody(string $body): array
    {
        $text = trim($body);

        $decoded = $this->tryBase64Decode($text);
        if ($decoded !== null && $this->looksLikeLinkList($decoded)) {
            $text = trim($decoded);
        }

        $lines = preg_split('/\r\n|\r|\n/', $text) ?: [];
        $outbounds = [];
        $identities = [];
        $seen = [];

        foreach ($lines as $line) {
            $line = trim($line);
            if ($line === '' || str_starts_with($line, '#')) {
                continue;
            }
            try {
                $parsed = OutboundLinkParser::parse($line);
            } catch (\InvalidArgumentException) {
                continue; // 不支持/坏链接：跳过，不影响其它
            }

            $tag = (string) ($parsed['tag'] ?? '');
            if (in_array($tag, self::RESERVED_TAGS, true)) {
                continue; // 占保留 tag 的链接丢弃，防污染内置出站
            }

            $base = $this->identityFor($line, $parsed);
            $n = $seen[$base] ?? 0;
            $identity = $n > 0 ? $base . '#' . $n : $base;
            $seen[$base] = $n + 1;

            $outbounds[] = [
                'protocol' => $parsed['protocol'],
                'settings' => $parsed['settings'],
                'stream_settings' => $parsed['stream_settings'] ?? [],
                'tag' => $tag,
            ];
            $identities[] = $identity;

            if (count($outbounds) >= self::MAX_OUTBOUNDS) {
                break;
            }
        }

        return ['outbounds' => $outbounds, 'identities' => $identities];
    }

    /**
     * 链接的稳定 identity（跨刷新、忽略 remark 改名）：
     *  - vmess：内层 JSON 去 ps 后规范化
     *  - 其余：协议 + 去掉 fragment 的原始链接
     */
    private function identityFor(string $link, array $parsed): string
    {
        if (($parsed['protocol'] ?? '') === 'vmess') {
            $json = $this->b64decodeLoose(substr($link, strlen('vmess://')));
            $j = json_decode((string) $json, true);
            if (is_array($j)) {
                unset($j['ps']);
                ksort($j);

                return 'vmess:' . (string) json_encode($j, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE);
            }
        }

        $scheme = strtolower((string) parse_url($link, PHP_URL_SCHEME));
        $stripped = explode('#', $link, 2)[0];

        return $scheme . '|' . $stripped;
    }

    /**
     * 标签稳定分配（纯函数，可单测）：
     *  1) identity 见过 → 复用旧 tag
     *  2) 否则位置兜底复用（未被其它 identity 预留时）
     *  3) 否则 prefix + slug(remark) 新分配
     * 批内用 -N 去重。返回按序的 tag 数组。
     *
     * @param  array<int,array>  $outbounds
     * @param  array<int,string>  $identities
     * @param  array<string,string>  $prev  identity → tag
     * @param  array<int,string>  $prevTagByIndex
     * @return array<int,string>
     */
    public static function assignStableTags(
        array $outbounds,
        array $identities,
        array $prev,
        array $prevTagByIndex,
        int $subId,
        string $prefix,
    ): array {
        $reserved = [];
        foreach ($outbounds as $i => $_) {
            $id = $identities[$i] ?? '';
            if ($id !== '' && ($prev[$id] ?? '') !== '') {
                $reserved[$prev[$id]] = true;
            }
        }

        $used = [];
        $assigned = [];
        foreach ($outbounds as $i => $ob) {
            $id = $identities[$i] ?? '';
            $candidate = '';
            $identityTag = '';

            if ($id !== '' && ($prev[$id] ?? '') !== '') {
                $candidate = $prev[$id];
                $identityTag = $prev[$id];
            }
            if ($candidate === '' && ($prevTagByIndex[$i] ?? '') !== '' && empty($reserved[$prevTagByIndex[$i]])) {
                $candidate = $prevTagByIndex[$i];
            }
            if ($candidate === '') {
                $p = $prefix !== '' ? $prefix : 'sub' . $subId . '-';
                $candidate = self::suggestTag($p, (string) ($ob['tag'] ?? ''), $i);
            }

            $final = $candidate;
            for ($k = 1; isset($used[$final]) || (isset($reserved[$final]) && $final !== $identityTag); $k++) {
                $final = $candidate . '-' . $k;
            }
            $used[$final] = true;
            $assigned[$i] = $final;
        }

        return $assigned;
    }

    /** 由 prefix + remark 生成 tag（remark 空则用序号兜底）。 */
    public static function suggestTag(string $prefix, string $remark, int $idx): string
    {
        $base = self::slug($remark);
        if ($base === '') {
            $base = (string) $idx;
        }
        $p = rtrim($prefix, '-');

        return $p !== '' ? $p . '-' . $base : $base;
    }

    /** remark → slug（保留字母数字与 CJK，其余折叠成 '-'）。 */
    private static function slug(string $remark): string
    {
        $s = mb_strtolower(trim($remark), 'UTF-8');
        $s = (string) preg_replace('/[^\p{L}\p{N}]+/u', '-', $s);
        $s = trim($s, '-');

        return mb_substr($s, 0, 48);
    }

    // ── SSRF ─────────────────────────────────────────────

    /** 主机名必须解析到公网地址（直连 IP 直接判定）。 */
    private function assertPublicHost(string $host, string $scheme): void
    {
        if (! in_array(strtolower($scheme), ['http', 'https'], true)) {
            throw new \InvalidArgumentException("不支持的协议：{$scheme}");
        }

        $host = trim($host);
        // parse_url 对 IPv6 字面量给出带方括号的 host（[::1]），剥掉再判定
        if (str_starts_with($host, '[') && str_ends_with($host, ']')) {
            $host = substr($host, 1, -1);
        }
        if ($host === '') {
            throw new \InvalidArgumentException('订阅地址缺少主机名');
        }

        // 直连 IP：直接判定
        if (filter_var($host, FILTER_VALIDATE_IP) !== false) {
            if ($this->isBlockedIp($host)) {
                throw new \InvalidArgumentException("订阅地址指向内网/保留地址：{$host}");
            }

            return;
        }

        // 主机名：解析 A/AAAA，任一为内网/保留即拒（DNS 解析失败也拒，无法确认安全）
        $ips = $this->resolveHost($host);
        if ($ips === []) {
            throw new \InvalidArgumentException("订阅地址主机名无法解析：{$host}");
        }
        foreach ($ips as $ip) {
            if ($this->isBlockedIp($ip)) {
                throw new \InvalidArgumentException("订阅地址解析到内网/保留地址：{$host} → {$ip}");
            }
        }
    }

    /** @return string[] */
    private function resolveHost(string $host): array
    {
        if (self::$hostResolver !== null) {
            return array_values(array_filter((array) (self::$hostResolver)($host), 'is_string'));
        }

        $ips = [];
        if (function_exists('gethostbynamel')) {
            $a = @gethostbynamel($host);
            if (is_array($a)) {
                $ips = array_merge($ips, $a);
            }
        }
        if (function_exists('dns_get_record')) {
            $aaaa = @dns_get_record($host, DNS_AAAA);
            if (is_array($aaaa)) {
                foreach ($aaaa as $rec) {
                    if (! empty($rec['ipv6'])) {
                        $ips[] = (string) $rec['ipv6'];
                    }
                }
            }
        }

        return array_values(array_unique($ips));
    }

    /**
     * 是否为内网/保留/回环/链路本地/CGNAT/组播/未指定地址（含 IPv4-mapped IPv6）。
     * 口径参照 3x-ui internal/util/netsafe/netsafe.go::IsBlockedIP。
     */
    private function isBlockedIp(string $ip): bool
    {
        if (filter_var($ip, FILTER_VALIDATE_IP, FILTER_FLAG_IPV4) !== false) {
            $long = ip2long($ip);
            if ($long === false) {
                return true;
            }
            $b = [($long >> 24) & 0xFF, ($long >> 16) & 0xFF, ($long >> 8) & 0xFF, $long & 0xFF];

            if ($b[0] === 0) {
                return true; // 0.0.0.0/8（含未指定 0.0.0.0）
            }
            if ($b[0] === 10) {
                return true; // 10/8
            }
            if ($b[0] === 100 && $b[1] >= 64 && $b[1] <= 127) {
                return true; // 100.64/10 CGNAT
            }
            if ($b[0] === 127) {
                return true; // 127/8 回环
            }
            if ($b[0] === 169 && $b[1] === 254) {
                return true; // 169.254/16 链路本地
            }
            if ($b[0] === 172 && $b[1] >= 16 && $b[1] <= 31) {
                return true; // 172.16/12
            }
            if ($b[0] === 192 && $b[1] === 0 && $b[2] === 0) {
                return true; // 192.0.0/24
            }
            if ($b[0] === 192 && $b[1] === 0 && $b[2] === 2) {
                return true; // 192.0.2/24 TEST-NET-1
            }
            if ($b[0] === 192 && $b[1] === 168) {
                return true; // 192.168/16
            }
            if ($b[0] === 198 && ($b[1] === 18 || $b[1] === 19)) {
                return true; // 198.18/15 基准测试
            }
            if ($b[0] === 198 && $b[1] === 51 && $b[2] === 100) {
                return true; // 198.51.100/24 TEST-NET-2
            }
            if ($b[0] === 203 && $b[1] === 0 && $b[2] === 113) {
                return true; // 203.0.113/24 TEST-NET-3
            }
            if ($b[0] >= 224 && $b[0] <= 239) {
                return true; // 224/4 组播
            }
            if ($b[0] >= 240) {
                return true; // 240/4 保留（含 255.255.255.255 广播）
            }

            return false;
        }

        if (filter_var($ip, FILTER_VALIDATE_IP, FILTER_FLAG_IPV6) !== false) {
            $bin = inet_pton($ip);
            if ($bin === false) {
                return true;
            }
            $isZero = str_repeat("\x00", 16) === $bin;
            if ($isZero) {
                return true; // :: 未指定
            }
            if ($bin === str_repeat("\x00", 15) . "\x01") {
                return true; // ::1 回环
            }
            // ::ffff:x.x.x.x IPv4-mapped → 按内嵌 v4 判定
            if (substr($bin, 0, 10) === str_repeat("\x00", 10) && substr($bin, 10, 2) === "\xff\xff") {
                return $this->isBlockedIp((string) inet_ntop(substr($bin, 12, 4)));
            }
            $first = ord($bin[0]);
            if (($first & 0xFE) === 0xFC) {
                return true; // fc00::/7 唯一本地
            }
            if ($first === 0xFE && (ord($bin[1]) & 0xC0) === 0x80) {
                return true; // fe80::/10 链路本地
            }

            return false;
        }

        // 无法解析为 IP：视为不安全
        return true;
    }

    // ── 内部工具 ─────────────────────────────────────────

    private function recordError(XrayOutboundSubscription $sub, string $message): void
    {
        $sub->forceFill([
            'last_status' => 'failed',
            'last_fetched_at' => now(),
            'last_error' => mb_substr($message, 0, 490),
        ])->save();
    }

    /** base64 宽松解码（标准/URL-safe、缺 padding），失败 null。 */
    private function tryBase64Decode(string $value): ?string
    {
        $value = str_replace(["\r", "\n", ' ', "\t"], '', $value);
        $value = strtr($value, '-_', '+/');
        $pad = strlen($value) % 4;
        if ($pad === 1) {
            return null;
        }
        if ($pad > 0) {
            $value .= str_repeat('=', 4 - $pad);
        }
        if (! preg_match('/^[A-Za-z0-9+\/=]+$/', $value)) {
            return null;
        }
        $decoded = base64_decode($value, true);

        return $decoded === false ? null : $decoded;
    }

    /** 判断字符串是否像「多行节点链接清单」。 */
    private function looksLikeLinkList(string $s): bool
    {
        return (bool) preg_match('/^(vless|vmess|trojan|ss|socks|http):\/\//mi', $s);
    }

    /** 宽容 base64 解码（标准/URL-safe、有无填充都收）。 */
    private function b64decodeLoose(string $input): string|false
    {
        $input = trim($input);
        $normalized = strtr($input, '-_', '+/');
        $pad = strlen($normalized) % 4;
        if ($pad > 0) {
            $normalized .= str_repeat('=', 4 - $pad);
        }

        return base64_decode($normalized, true);
    }
}
