<?php

namespace App\Drivers\Xray;

use App\Models\XrayOutbound;

/**
 * 出站 → 客户端分享链接（vless / vmess / trojan / ss / socks / http）。
 *
 * 与 {@see OutboundLinkParser::parse()} **严格对称**：这里生成的链接贴回解析器，
 * 能还原出等价的 settings / stream_settings 结构（各协议用例覆盖）。
 *
 * 出站数据结构（xray 客户端口径，与解析器输出一致）：
 *   - vless/vmess：settings.vnext[0].{address,port,users[0].{id,flow,alterId,security}}
 *   - trojan：settings.servers[0].{address,port,password}
 *   - shadowsocks：settings.servers[0].{address,port,method,password}
 *   - socks/http：settings.servers[0].{address,port,users[0].{user,pass}}
 * 传输参数在 stream_settings（reality / tls / ws / xhttp / grpc）。
 *
 * wireguard / freedom / blackhole 无标准分享链接 → 抛 InvalidArgumentException
 * （调用侧据此提示「不支持」，按钮禁用）。
 */
class OutboundLinkGenerator
{
    /** 能转成分享链接的协议。 */
    public const SUPPORTED = ['vless', 'vmess', 'trojan', 'shadowsocks', 'socks', 'http'];

    /**
     * 生成出站分享链接。`#fragment` 用出站备注（有则用）否则用标识，便于辨认。
     *
     * @throws \InvalidArgumentException 协议不支持 / 目标信息缺失
     */
    public static function generate(XrayOutbound $outbound): string
    {
        $protocol = (string) $outbound->protocol;
        if (! in_array($protocol, self::SUPPORTED, true)) {
            throw new \InvalidArgumentException("该出站类型（{$protocol}）没有标准分享链接");
        }

        $settings = is_array($outbound->settings) ? $outbound->settings : [];
        $stream = is_array($outbound->stream_settings) ? $outbound->stream_settings : [];
        $first = $settings['vnext'][0] ?? $settings['servers'][0] ?? null;
        if (! is_array($first) || (string) ($first['address'] ?? '') === '') {
            throw new \InvalidArgumentException('该出站没有可导出的目标地址');
        }

        $name = self::displayName($outbound);

        return match ($protocol) {
            'vless' => self::vlessUri($first, $stream, $name),
            'vmess' => self::vmessUri($first, $stream, $name),
            'trojan' => self::trojanUri($first, $stream, $name),
            'shadowsocks' => self::ssUri($first, $name),
            'socks' => self::socksLikeUri($first, $name, false),
            'http' => self::socksLikeUri($first, $name, true),
            default => throw new \InvalidArgumentException("不支持的协议：{$protocol}"),
        };
    }

    /** 链接显示名：备注（有则用）否则用标识。 */
    public static function displayName(XrayOutbound $outbound): string
    {
        $remark = trim((string) $outbound->remark);

        return $remark !== '' ? $remark : (string) $outbound->tag;
    }

    private static function vlessUri(array $first, array $stream, string $name): string
    {
        $address = (string) $first['address'];
        $port = (int) ($first['port'] ?? 443);
        $user = is_array($first['users'][0] ?? null) ? $first['users'][0] : [];
        $uuid = rawurlencode((string) ($user['id'] ?? ''));

        $network = (string) ($stream['network'] ?? 'tcp');
        $security = (string) ($stream['security'] ?? 'none');

        $query = 'type=' . rawurlencode($network) . '&security=' . rawurlencode($security);

        if ($security === 'reality') {
            $reality = is_array($stream['realitySettings'] ?? null) ? $stream['realitySettings'] : [];
            $query .= '&fp=' . rawurlencode((string) ($reality['fingerprint'] ?? '') ?: 'chrome');
            $query .= '&sni=' . rawurlencode((string) ($reality['serverName'] ?? ''));
            $query .= '&pbk=' . rawurlencode((string) ($reality['publicKey'] ?? ''));
            if (! empty($reality['shortId'])) {
                $query .= '&sid=' . rawurlencode((string) $reality['shortId']);
            }
            if (! empty($reality['spiderX'])) {
                $query .= '&spx=' . rawurlencode((string) $reality['spiderX']);
            }
        } elseif ($security === 'tls') {
            $tls = is_array($stream['tlsSettings'] ?? null) ? $stream['tlsSettings'] : [];
            $query .= '&fp=chrome&sni=' . rawurlencode((string) ($tls['serverName'] ?? $address));
        } else {
            $query .= '&fp=chrome';
        }

        $query .= self::transportQuery($network, $stream);

        if (! empty($user['flow'])) {
            $query .= '&flow=' . rawurlencode((string) $user['flow']);
        }

        return sprintf('vless://%s@%s:%d?%s#%s', $uuid, self::host($address), $port, $query, rawurlencode($name));
    }

    private static function vmessUri(array $first, array $stream, string $name): string
    {
        $user = is_array($first['users'][0] ?? null) ? $first['users'][0] : [];
        $network = (string) ($stream['network'] ?? 'tcp');
        $security = (string) ($stream['security'] ?? 'none');
        $tls = $security === 'tls';
        $ws = is_array($stream['wsSettings'] ?? null) ? $stream['wsSettings'] : [];
        $xhttp = is_array($stream['xhttpSettings'] ?? null) ? $stream['xhttpSettings'] : [];
        $tlsSettings = is_array($stream['tlsSettings'] ?? null) ? $stream['tlsSettings'] : [];

        $payload = [
            'v' => '2',
            'ps' => $name,
            'add' => (string) $first['address'],
            'port' => (string) ($first['port'] ?? 443),
            'id' => (string) ($user['id'] ?? ''),
            'aid' => (string) ($user['alterId'] ?? 0),
            'scy' => (string) ($user['security'] ?? 'auto'),
            'net' => $network,
            'type' => 'none',
            'host' => (string) ($ws['headers']['Host'] ?? ($xhttp['host'] ?? '')),
            'path' => (string) ($ws['path'] ?? ($xhttp['path'] ?? '')),
            'tls' => $tls ? 'tls' : '',
            'sni' => $tls ? (string) ($tlsSettings['serverName'] ?? '') : '',
        ];

        return 'vmess://' . base64_encode(
            (string) json_encode($payload, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES)
        );
    }

    private static function trojanUri(array $first, array $stream, string $name): string
    {
        $address = (string) $first['address'];
        $port = (int) ($first['port'] ?? 443);
        $password = rawurlencode((string) ($first['password'] ?? ''));

        $network = (string) ($stream['network'] ?? 'tcp');
        $security = (string) ($stream['security'] ?? 'none');

        $query = 'security=' . ($security === 'tls' ? 'tls' : 'none') . '&type=' . rawurlencode($network);
        if ($security === 'tls') {
            $tls = is_array($stream['tlsSettings'] ?? null) ? $stream['tlsSettings'] : [];
            $query .= '&sni=' . rawurlencode((string) ($tls['serverName'] ?? $address));
        }
        $query .= self::transportQuery($network, $stream);

        return sprintf('trojan://%s@%s:%d?%s#%s', $password, self::host($address), $port, $query, rawurlencode($name));
    }

    private static function ssUri(array $first, string $name): string
    {
        $address = (string) $first['address'];
        $port = (int) ($first['port'] ?? 443);
        $method = (string) ($first['method'] ?? '');
        $password = (string) ($first['password'] ?? '');

        // SIP002：ss://base64(method:password)@host:port#name
        $userinfo = base64_encode($method . ':' . $password);

        return sprintf('ss://%s@%s:%d#%s', $userinfo, self::host($address), $port, rawurlencode($name));
    }

    private static function socksLikeUri(array $first, string $name, bool $http): string
    {
        $address = (string) $first['address'];
        $port = (int) ($first['port'] ?? ($http ? 8080 : 1080));
        $user = is_array($first['users'][0] ?? null) ? $first['users'][0] : [];
        $username = (string) ($user['user'] ?? '');
        $password = (string) ($user['pass'] ?? '');

        $cred = '';
        if ($username !== '' || $password !== '') {
            $cred = rawurlencode($username) . ':' . rawurlencode($password) . '@';
        }

        return sprintf('%s://%s%s:%d#%s', $http ? 'http' : 'socks', $cred, self::host($address), $port, rawurlencode($name));
    }

    /** 传输层查询串（ws / xhttp / grpc）——与 OutboundLinkParser::applyTransport 对称。 */
    private static function transportQuery(string $network, array $stream): string
    {
        if ($network === 'tcp') {
            // TCP header 伪装（http）→ headerType/path/host；host 口径与 3x-ui 订阅一致（response 优先）
            $tcp = is_array($stream['tcpSettings'] ?? null) ? $stream['tcpSettings'] : [];
            $header = is_array($tcp['header'] ?? null) ? $tcp['header'] : [];
            if (($header['type'] ?? '') === 'http') {
                $request = is_array($header['request'] ?? null) ? $header['request'] : [];
                $response = is_array($header['response'] ?? null) ? $header['response'] : [];
                $query = '&headerType=http&path=' . rawurlencode((string) ($request['path'][0] ?? '/'));
                $host = (string) ($response['headers']['Host'][0] ?? ($request['headers']['Host'][0] ?? ''));
                if ($host !== '') {
                    $query .= '&host=' . rawurlencode($host);
                }

                return $query;
            }

            return '';
        }

        if ($network === 'ws') {
            $ws = is_array($stream['wsSettings'] ?? null) ? $stream['wsSettings'] : [];
            $query = '&path=' . rawurlencode((string) ($ws['path'] ?? '/'));
            $host = (string) ($ws['headers']['Host'] ?? '');
            if ($host !== '') {
                $query .= '&host=' . rawurlencode($host);
            }

            return $query;
        }

        if ($network === 'xhttp') {
            $xhttp = is_array($stream['xhttpSettings'] ?? null) ? $stream['xhttpSettings'] : [];

            return self::xhttpQuery($xhttp);
        }

        if ($network === 'httpupgrade') {
            $hu = is_array($stream['httpupgradeSettings'] ?? null) ? $stream['httpupgradeSettings'] : [];
            $query = '&path=' . rawurlencode((string) ($hu['path'] ?? '/'));
            if (! empty($hu['host'])) {
                $query .= '&host=' . rawurlencode((string) $hu['host']);
            }

            return $query;
        }

        if ($network === 'grpc') {
            $grpc = is_array($stream['grpcSettings'] ?? null) ? $stream['grpcSettings'] : [];
            $query = '';
            if (! empty($grpc['serviceName'])) {
                $query .= '&serviceName=' . rawurlencode((string) $grpc['serviceName']);
            }
            if (! empty($grpc['authority'])) {
                $query .= '&authority=' . rawurlencode((string) $grpc['authority']);
            }

            return $query;
        }

        return '';
    }

    /**
     * xhttp 传输的查询串（与 3x-ui 的双编码对齐）。
     *
     * path/host/mode 之外的高级字段（如 xPaddingBytes）编码为：
     *   - extra=<urlencoded-json>：xray-core 系客户端（v2rayNG / Happ 等）读取的完整 blob
     *   - x_padding_bytes=<range>：sing-box 系客户端的 flat 参数
     * 无高级字段时两者都不产出，老配置链接字节不变。
     * 与 {@see OutboundLinkParser::applyTransport()} 的 xhttp 分支对称。
     */
    public static function xhttpQuery(array $xhttp): string
    {
        $query = '&path=' . rawurlencode((string) ($xhttp['path'] ?? '/'));
        if (! empty($xhttp['host'])) {
            $query .= '&host=' . rawurlencode((string) $xhttp['host']);
        }
        if (! empty($xhttp['mode'])) {
            $query .= '&mode=' . rawurlencode((string) $xhttp['mode']);
        }

        // extra 白名单（与 3x-ui sub/service.go buildXhttpExtra 对齐）：只带双向字段。
        // 服务端专属（noSSEHeader/scMaxBufferedPosts/scStreamUpServerSecs/serverMaxHeaderBytes）
        // 不进链接；等于内核默认的 sc* 值剔除（scMinPostsIntervalMs=30 是已知 DPI 指纹 #5141）。
        // mode 已在 query 参数中表达，不再重复进 extra——无高级字段时链接与旧版逐字节一致。
        $ext = [];
        $padding = trim((string) ($xhttp['xPaddingBytes'] ?? ''));
        if ($padding !== '') {
            $ext['xPaddingBytes'] = $padding;
        }
        if (! empty($xhttp['xPaddingObfsMode'])) {
            $ext['xPaddingObfsMode'] = true;
            foreach (['xPaddingKey', 'xPaddingHeader', 'xPaddingPlacement', 'xPaddingMethod'] as $field) {
                $v = trim((string) ($xhttp[$field] ?? ''));
                if ($v !== '') {
                    $ext[$field] = $v;
                }
            }
        }
        $coreDefaults = ['scMaxEachPostBytes' => '1000000', 'scMinPostsIntervalMs' => '30'];
        foreach (['uplinkHTTPMethod', 'sessionIDPlacement', 'sessionIDKey', 'sessionIDTable', 'sessionIDLength', 'seqPlacement', 'seqKey', 'uplinkDataPlacement', 'uplinkDataKey', 'scMaxEachPostBytes', 'scMinPostsIntervalMs'] as $field) {
            $v = trim((string) ($xhttp[$field] ?? ''));
            if ($v === '' || $v === ($coreDefaults[$field] ?? null)) {
                continue;
            }
            $ext[$field] = $v;
        }
        if (! empty($xhttp['uplinkChunkSize'])) {
            $ext['uplinkChunkSize'] = (int) $xhttp['uplinkChunkSize'];
        }
        if (! empty($xhttp['noGRPCHeader'])) {
            $ext['noGRPCHeader'] = true;
        }
        if ($ext !== []) {
            $query .= '&extra=' . rawurlencode((string) json_encode($ext, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES));
        }
        if ($padding !== '') {
            $query .= '&x_padding_bytes=' . rawurlencode($padding);
        }

        return $query;
    }

    /** 目标主机（IPv6 加方括号，parse_url 才认）。 */
    private static function host(string $address): string
    {
        $address = trim($address);
        if (str_contains($address, ':') && ! str_starts_with($address, '[')) {
            return '[' . $address . ']';
        }

        return $address;
    }
}
