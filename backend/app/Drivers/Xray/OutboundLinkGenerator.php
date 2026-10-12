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
            $query = '&path=' . rawurlencode((string) ($xhttp['path'] ?? '/'));
            if (! empty($xhttp['host'])) {
                $query .= '&host=' . rawurlencode((string) $xhttp['host']);
            }
            if (! empty($xhttp['mode'])) {
                $query .= '&mode=' . rawurlencode((string) $xhttp['mode']);
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
