<?php

namespace App\Services\Webshare;

use Illuminate\Http\Client\ConnectionException;
use Illuminate\Support\Facades\Http;
use Illuminate\Support\Facades\Log;

/**
 * Webshare 上游 API 客户端（v2）。
 *
 * 只做「一次请求 + 把失败讲清楚」，不含业务语义：
 *   - 鉴权：Authorization: Token <api_key>
 *   - 分页：/subuser/ 返回 {count,next,previous,results:[...]}，这里拆出 results
 *   - 429 限流 / 403 未开通子用户权限 → 抛 WebshareException，带状态码与 Retry-After
 *
 * 文档：https://proxy.webshare.io/api/v2
 */
class WebshareClient
{
    public const BASE_URL = 'https://proxy.webshare.io/api/v2';

    private const TIMEOUT = 15;
    private const CONNECT_TIMEOUT = 6;

    public function __construct(
        private readonly string $apiToken,
        private readonly string $baseUrl = self::BASE_URL,
        // 出口代理（socks5h://host:port / http://host:port），空 = 直连
        private readonly ?string $proxy = null,
        // 自定义 CA bundle（PHP 未配 curl.cainfo 时用），空 = 系统 CA
        private readonly ?string $caBundle = null,
    ) {}

    /** 套餐列表（含 proxy_limit 单位 GB、countries 国家池）。 */
    public function listPlans(): array
    {
        $data = $this->request('GET', '/subscription/plan/');

        return $data['results'] ?? $data;
    }

    /** 子用户列表（自动展开分页 results）。 */
    public function listSubusers(): array
    {
        $data = $this->request('GET', '/subuser/');

        return $data['results'] ?? $data;
    }

    /**
     * 代理 IP 列表（账号/套餐级的出口 IP，不是子用户凭据）。
     *
     * 返回 {count, next, previous, results:[{proxy_address, port, country_code, city_name,
     * asn_name, valid, last_verification, username, password}]}。
     * ⚠️ results 里的 username/password 是**主账号**的代理凭据，调用方必须过滤掉，
     * 绝不能出现在管理端响应里（面板只展示 IP/端口/国家）。
     *
     * @param array{mode?:string, plan_id?:int, page?:int, page_size?:int} $query
     */
    public function listProxies(array $query = []): array
    {
        $query = array_filter($query, static fn ($v) => $v !== null && $v !== '');
        $qs = http_build_query($query, '', '&', PHP_QUERY_RFC3986);

        return $this->request('GET', '/proxy/list/' . ($qs !== '' ? '?' . $qs : ''));
    }

    /**
     * 单个子用户详情。
     *
     * ⚠️ 实测（2026-10-23）：只读详情**不含 username / password** —— 凭据只在
     * `createSubuser()` 的响应里给一次（创建后拿不回来，丢了只能改密码重发）。
     * 所以本方法只适合读用量/额度，别指望用它同步账号凭据。
     *
     * 返回体里 `aggregate_stats.bandwidth_total`（字节）是用量口径。
     */
    public function getSubuser(string $upstreamId): array
    {
        return $this->request('GET', '/subuser/' . rawurlencode($upstreamId) . '/');
    }

    /**
     * 新建子用户。$attrs 只传 Webshare 文档里建号支持的字段：
     * label / proxy_limit(GB) / max_thread_count。
     */
    public function createSubuser(array $attrs): array
    {
        return $this->request('POST', '/subuser/', $attrs);
    }

    public function updateSubuser(string $upstreamId, array $attrs): array
    {
        return $this->request('PATCH', '/subuser/' . rawurlencode($upstreamId) . '/', $attrs);
    }

    /** 删除子用户（上游返回 204 无响应体）。 */
    public function deleteSubuser(string $upstreamId): void
    {
        $this->request('DELETE', '/subuser/' . rawurlencode($upstreamId) . '/');
    }

    /**
     * 发一次请求并解包 JSON。非 2xx 一律抛 WebshareException（含状态码），
     * 网络层失败（DNS/超时/连不上）也包成本异常，调用方只看一种失败类型。
     */
    private function request(string $method, string $path, array $payload = []): array
    {
        try {
            $http = Http::withToken($this->apiToken, 'Token')
                ->timeout(self::TIMEOUT)
                ->connectTimeout(self::CONNECT_TIMEOUT)
                ->acceptJson();

            // 代理 / 自定义 CA 只在配置了才加：都不配时保持 Guzzle 默认（直连 + 系统 CA）
            $options = [];
            if ($this->proxy !== null && $this->proxy !== '') {
                $options['proxy'] = $this->proxy;
            }
            if ($this->caBundle !== null && $this->caBundle !== '') {
                $options['verify'] = $this->caBundle;
            }
            if ($options !== []) {
                $http = $http->withOptions($options);
            }

            $response = $http->send($method, rtrim($this->baseUrl, '/') . $path, [
                'json' => $payload,
            ]);
        } catch (ConnectionException $e) {
            // 原始错误是 cURL error 28/7 之类的机器话，管理员看到只会一脸问号。
            // 这里翻译成「哪一段不通 + 该去查什么」：出口代理挂了是国内最常见的失败原因，
            // 直连不通在国内机房也很常见。原始信息进日志，接口只给人话。
            Log::warning('Webshare 网络层失败', [
                'proxy' => $this->proxy ?: '(直连)',
                'error' => $e->getMessage(),
            ]);

            $raw = $e->getMessage();
            $reason = (str_contains($raw, 'timed out') || str_contains($raw, 'error 28')) ? '超时' : '失败';
            $via = ($this->proxy !== null && $this->proxy !== '')
                ? "出口代理 {$this->proxy} 不可用（代理客户端可能没连上节点 / 节点已失效）"
                : '当前是直连，国内网络通常出不去（可在 config/panel.php 配置 webshare.proxy）';

            throw new WebshareException("连接 Webshare {$reason}：{$via}。");
        }

        if ($response->status() === 429) {
            throw new WebshareException(
                'Webshare 请求过于频繁（429），请稍后再试',
                429,
                $this->retryAfter($response->header('Retry-After')),
            );
        }

        if ($response->status() === 403) {
            throw new WebshareException(
                'Webshare 拒绝访问（403）：请在 proxy.webshare.io/subuser/ 开通子用户 API 权限',
                403,
            );
        }

        if (!$response->successful()) {
            throw new WebshareException(
                $this->errorMessage($response->json(), $response->status()),
                $response->status(),
            );
        }

        // 204 无响应体（DELETE）
        if (trim((string) $response->body()) === '') {
            return [];
        }

        $data = $response->json();

        if (!is_array($data)) {
            throw new WebshareException('Webshare 返回了无法解析的响应', $response->status());
        }

        return $data;
    }

    /**
     * 上游的错误体形状不统一，常见三种：
     *   {"detail": "..."}
     *   {"non_field_errors": [{"message": "...", "code": "..."}]}   ← 实测建号超额就是这个
     *   {"label": ["..."]}
     *
     * 早前版本对数组元素直接 strval()，遇到上面第二种会得到 "Array"（还抛 PHP warning），
     * 管理员看不到真实原因（例如「子用户额度已满」）。这里改成递归取第一段可读文本。
     */
    private function errorMessage(mixed $body, int $status): string
    {
        $text = $this->firstErrorText($body);
        if ($text !== '') {
            return mb_substr($text, 0, 250);
        }

        return "Webshare 请求失败（HTTP {$status}）";
    }

    /**
     * 从错误体里挖出第一段人类可读的文本。
     *
     * 优先 document 里已知的说明键，其次按顺序取第一个能提出文本的元素；
     * 深度上限 3 层，防止异常结构把栈撑爆。
     */
    private function firstErrorText(mixed $value, int $depth = 0): string
    {
        if ($depth > 3) {
            return '';
        }
        if (is_string($value)) {
            return trim($value);
        }
        if (! is_array($value)) {
            return '';
        }

        foreach (['detail', 'message', 'non_field_errors', 'error', 'label'] as $key) {
            if (! array_key_exists($key, $value)) {
                continue;
            }
            $found = $this->firstErrorText($value[$key], $depth + 1);
            if ($found !== '') {
                return $found;
            }
        }

        // 兜底：任何一段能读出文本的值（多段用分号拼，最多 3 段）
        $parts = [];
        foreach ($value as $item) {
            $found = $this->firstErrorText($item, $depth + 1);
            if ($found !== '') {
                $parts[] = $found;
                if (count($parts) >= 3) {
                    break;
                }
            }
        }

        return implode('；', $parts);
    }

    private function retryAfter(?string $header): ?int
    {
        if ($header === null || $header === '' || !is_numeric($header)) {
            return null;
        }

        return max(0, (int) $header);
    }
}
