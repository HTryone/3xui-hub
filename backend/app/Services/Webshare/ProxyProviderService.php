<?php

namespace App\Services\Webshare;

use App\Models\ProxyAccount;
use App\Models\ProxyProvider;
use App\Models\User;
use Illuminate\Support\Collection;

/**
 * 代理上游业务层：把 Webshare 的 subuser 变成平台用户可用的代理凭据。
 *
 * 关键产品约束（来自 Webshare API 文档，不是实现取舍）：
 *  - 子用户没有启用/禁用开关 —— 停用只能「改随机密码 + proxy_limit 降到 0.01」，
 *    恢复时由调用方重新下发新密码（本服务不保存旧密码）。
 *  - 子用户 API 需要站长在 Webshare 侧开通，否则一律 403（WebshareException 会区分）。
 *  - aggregate_stats.bandwidth_total 是主账号账单周期口径，这里只做「字节落库」，
 *    周期对齐由上层决定（proxy_accounts 有 last_synced_at 记录回采时刻）。
 */
class ProxyProviderService
{
    /** 停用时把带宽上限压到 GB 计量下的最小值（不是 0：0 在 Webshare 表示不限量）。 */
    private const DISABLED_PROXY_LIMIT = 0.01;

    /** 列表/详情接口回给前端时，代理国家池只带国家与数量，不带任何凭据。 */
    public function providerSummary(ProxyProvider $provider): array
    {
        return [
            'id' => $provider->id,
            'name' => $provider->name,
            'driver' => $provider->driver,
            'enabled' => (bool) $provider->enabled,
            'last_status' => $provider->last_status,
            'last_error' => $provider->last_error,
            'last_checked_at' => $provider->last_checked_at?->toIso8601String(),
            // 只回布尔：token 明文/密文都不出接口
            'has_token' => (string) $provider->api_token !== '',
            'account_count' => $provider->accounts()->count(),
        ];
    }

    public function accountSummary(ProxyAccount $account): array
    {
        return [
            'id' => $account->id,
            'provider_id' => $account->provider_id,
            'upstream_id' => $account->upstream_id,
            'user_id' => $account->user_id,
            'label' => $account->label,
            'username' => $account->username,
            // password 走 $hidden，这里也不展开
            'proxy_limit' => $account->proxy_limit,
            'max_thread_count' => $account->max_thread_count,
            'proxy_countries' => $account->proxy_countries ?? [],
            'used_bytes' => $account->used_bytes,
            'status' => $account->status,
            // 代理商城的号带这两项：卖家用来核对「谁买的、什么时候到期」
            'sku_id' => $account->sku_id,
            'expires_at' => $account->expires_at?->toIso8601String(),
            'last_synced_at' => $account->last_synced_at?->toIso8601String(),
        ];
    }

    /**
     * 连通性测试：打一次子用户列表。
     * 成功写 last_status=success；失败写 failed + 原因，并把异常继续抛给调用方
     * （接口要回原因，但不能因为一次测试失败就把账号删了/停用）。
     */
    public function test(ProxyProvider $provider): array
    {
        try {
            $subusers = $provider->client()->listSubusers();
        } catch (WebshareException $e) {
            $this->markProviderResult($provider, false, $e->getMessage());

            throw $e;
        }

        $this->markProviderResult($provider, true, null);

        return ['subuser_count' => count($subusers)];
    }

    /**
     * 拉套餐列表（不落库：套餐是站长在 Webshare 侧买的，面板只展示）。
     *
     * 字段口径以真实 `/subscription/plan/` 响应为准（2026-10-23 实测）：
     *   bandwidth_limit 套餐带宽（GB）、proxy_count 代理数、proxy_countries 国家池、
     *   subusers_available 还能开几个子用户（0 = 开不了，免费套餐就是 0）。
     * 上游**没有** name / proxy_limit / max_thread_count —— 那是子用户（subuser）的字段，
     * 早前按文档猜的映射会让套餐页全是 null。
     *
     * 只回 `status=active` 的套餐：实测账号里有 3 个 cancelled 套餐，它们的
     * `subusers_available` 仍是 3 这种**假数字**（套餐已取消，建号必然失败），
     * 原样展示会误导管理员按这个数去开号。取消/过期套餐对开号没有意义。
     */
    public function syncPlans(ProxyProvider $provider): array
    {
        $plans = $provider->client()->listPlans();

        $this->markProviderResult($provider, true, null);

        // 上游没给 status 时按 active 处理（向后兼容），给了就必须是 active
        $plans = array_values(array_filter(
            $plans,
            static fn ($plan) => ($plan['status'] ?? 'active') === 'active'
        ));

        return array_map(static fn ($plan) => [
            'id' => $plan['id'] ?? null,
            'status' => $plan['status'] ?? null,
            // 套餐带宽上限，单位 GB（免费套餐是 1.0）
            'bandwidth_limit' => $plan['bandwidth_limit'] ?? null,
            'proxy_count' => $plan['proxy_count'] ?? null,
            'proxy_type' => $plan['proxy_type'] ?? null,
            // 国家池：{国家代码: 代理数}
            'countries' => $plan['proxy_countries'] ?? [],
            // 还能开几个子用户；为 0 时建号必然被上游拒（subuser_limit_reached）
            'subusers_available' => $plan['subusers_available'] ?? null,
            'monthly_price' => $plan['monthly_price'] ?? null,
            'yearly_price' => $plan['yearly_price'] ?? null,
        ], $plans);
    }

    /**
     * 开子用户并落库。
     *
     * $attrs: label(必填) / proxy_limit(GB，0=不限量) / max_thread_count / user_id / proxy_countries
     * 建号请求只发 Webshare 支持的三个字段，其余（user_id / 国家池）是本地面板的事。
     *
     * @throws WebshareException 上游失败时不落库
     */
    public function createAccount(ProxyProvider $provider, array $attrs): ProxyAccount
    {
        $label = (string) ($attrs['label'] ?? '');
        if ($label === '') {
            throw new \InvalidArgumentException('label 不能为空');
        }

        $payload = [
            'label' => $label,
            'proxy_limit' => (float) ($attrs['proxy_limit'] ?? 0),
            'max_thread_count' => (int) ($attrs['max_thread_count'] ?? 0),
        ];

        $data = $provider->client()->createSubuser($payload);

        // 上游字段名以文档为准：id / username / password / proxy_limit / ...
        $upstreamId = (string) ($data['id'] ?? '');
        if ($upstreamId === '') {
            throw new WebshareException('Webshare 建号响应缺少 id');
        }

        $this->markProviderResult($provider, true, null);

        return ProxyAccount::create([
            'provider_id' => $provider->id,
            'upstream_id' => $upstreamId,
            'user_id' => $attrs['user_id'] ?? null,
            'label' => (string) ($data['label'] ?? $label),
            'username' => (string) ($data['username'] ?? ''),
            // encrypted cast：库里是密文，接口不回明文
            'password' => (string) ($data['password'] ?? ''),
            'proxy_limit' => (float) ($data['proxy_limit'] ?? $payload['proxy_limit']),
            'max_thread_count' => (int) ($data['max_thread_count'] ?? $payload['max_thread_count']),
            'proxy_countries' => $attrs['proxy_countries'] ?? ($data['proxy_countries'] ?? null),
            'used_bytes' => 0,
            'status' => 'active',
            'last_synced_at' => now(),
        ]);
    }

    /**
     * 停用：Webshare 没有启用/禁用开关，只能改随机密码把旧凭据废掉，
     * 并把 proxy_limit 压到 0.01 GB（不是 0 —— 0 表示不限量，正好相反）。
     * 本地同步成 fallback 密码，以免库里还留着可用的旧密码。
     */
    public function disableAccount(ProxyAccount $account): ProxyAccount
    {
        $newPassword = $this->randomPassword();

        $account->provider->client()->updateSubuser($account->upstream_id, [
            'password' => $newPassword,
            'proxy_limit' => self::DISABLED_PROXY_LIMIT,
        ]);

        $account->forceFill([
            'password' => $newPassword,
            'proxy_limit' => self::DISABLED_PROXY_LIMIT,
            'status' => 'disabled',
            'last_synced_at' => now(),
        ])->save();

        return $account;
    }

    /** 删除：先删上游（204），再从本地移除；上游已不存在（404）也把本地清掉。 */
    public function deleteAccount(ProxyAccount $account): void
    {
        try {
            $account->provider->client()->deleteSubuser($account->upstream_id);
        } catch (WebshareException $e) {
            // 上游已经没有这个子用户 → 本地残留属于脏数据，照删
            if ($e->status() !== 404) {
                throw $e;
            }
        }

        $account->delete();
    }

    /**
     * 回采用量：单个子用户的 aggregate_stats.bandwidth_total（字节）落库。
     * 返回本次回采到的字节数，方便批量调用方判断是否需要再刷页面。
     */
    public function pullUsage(ProxyAccount $account): int
    {
        $data = $account->provider->client()->getSubuser($account->upstream_id);

        $usedBytes = (int) ($data['aggregate_stats']['bandwidth_total'] ?? 0);

        $account->forceFill([
            'used_bytes' => $usedBytes,
            'last_synced_at' => now(),
        ])->save();

        return $usedBytes;
    }

    /**
     * 批量回采（容错：单条失败只记在返回结果里，不影响其它账号）。
     *
     * @param  Collection<int, ProxyAccount>|iterable<ProxyAccount>  $accounts
     * @return array{updated:int, failed:array<int,string>}
     */
    public function pullUsageBatch(iterable $accounts): array
    {
        $updated = 0;
        $failed = [];

        foreach ($accounts as $account) {
            try {
                $this->pullUsage($account);
                $updated++;
            } catch (WebshareException $e) {
                $failed[$account->id] = $e->getMessage();
            }
        }

        return ['updated' => $updated, 'failed' => $failed];
    }

    /**
     * 把上游已有子用户同步到本地（按 provider_id + upstream_id upsert）。
     *
     * 用于「站长在 Webshare 后台直接建过号」或换机器后本地库为空的情况：
     * 不同步的话，只有本面板开过的号才看得见。
     *
     * 上游只读接口**不返回 username / password**（凭据只在创建那一刻给一次），
     * 所以同步回来的记录没有凭据：页面显示「凭据未知」，要交付给用户必须先停用重置。
     * 已有记录的 status 不覆盖（避免把本地标记的 disabled 又拉回 active）。
     */
    public function syncAccounts(ProxyProvider $provider): array
    {
        $subusers = $provider->client()->listSubusers();

        $created = 0;
        $updated = 0;

        foreach ($subusers as $item) {
            $upstreamId = (string) ($item['id'] ?? '');
            if ($upstreamId === '') {
                continue;
            }

            $account = ProxyAccount::firstOrNew([
                'provider_id' => $provider->id,
                'upstream_id' => $upstreamId,
            ]);
            $isNew = ! $account->exists;

            $account->fill([
                'label' => (string) ($item['label'] ?? ''),
                'proxy_limit' => (float) ($item['proxy_limit'] ?? 0),
                'max_thread_count' => (int) ($item['max_thread_count'] ?? 0),
                'proxy_countries' => $item['proxy_countries'] ?? null,
                'used_bytes' => (int) ($item['aggregate_stats']['bandwidth_total'] ?? 0),
                'last_synced_at' => now(),
            ]);
            if ($isNew) {
                $account->status = 'active';
                // username 列 NOT NULL 且上游不返回凭据：显式空串（页面显示「凭据未知」）
                $account->username = '';
                $created++;
            } else {
                $updated++;
            }
            $account->save();
        }

        $this->markProviderResult($provider, true, null);

        return ['created' => $created, 'updated' => $updated, 'total' => count($subusers)];
    }

    /**
     * 代理 IP 列表（只展示给管理员的出口 IP，用于交付给用户）。
     *
     * 上游 `/proxy/list/` 的 results 里带**主账号**的 username/password，
     * 这里逐条白名单化，只留 IP / 端口 / 国家 / 城市 / ASN / 可用状态 ——
     * 主账号代理凭据绝不出接口（用户认证用的是子用户自己的账号密码）。
     *
     * @return array{items: array<int, array<string, mixed>>, total: int}
     */
    public function listProxies(ProxyProvider $provider, ?int $planId = null, int $pageSize = 100): array
    {
        $data = $provider->client()->listProxies([
            'mode' => 'direct',
            'plan_id' => $planId,
            'page_size' => $pageSize,
        ]);

        $items = array_map(static fn ($p) => [
            'id' => $p['id'] ?? null,
            'address' => $p['proxy_address'] ?? '',
            'port' => $p['port'] ?? null,
            'country' => $p['country_code'] ?? '',
            'city' => $p['city_name'] ?? '',
            'asn' => $p['asn_name'] ?? '',
            'valid' => (bool) ($p['valid'] ?? false),
            'last_verification' => $p['last_verification'] ?? null,
        ], $data['results'] ?? []);

        return ['items' => $items, 'total' => (int) ($data['count'] ?? count($items))];
    }

    private function markProviderResult(ProxyProvider $provider, bool $ok, ?string $error): void
    {
        $provider->forceFill([
            'last_status' => $ok ? 'success' : 'failed',
            'last_error' => $ok ? null : mb_substr((string) $error, 0, 250),
            'last_checked_at' => now(),
        ])->save();
    }

    /**
     * 停用用随机密码：不需要人记，只需要让旧凭据立刻失效。
     * 用 base64 里的 URL 安全子集，避免落库/展示时的转义歧义。
     */
    private function randomPassword(): string
    {
        return rtrim(strtr(base64_encode(random_bytes(24)), '+/', 'Aa'), '=');
    }

    /** 便捷：给某个平台用户开一个子用户。 */
    public function createAccountForUser(ProxyProvider $provider, User $user, array $attrs = []): ProxyAccount
    {
        return $this->createAccount($provider, array_merge($attrs, [
            'user_id' => $user->id,
            'label' => $attrs['label'] ?? ('u' . $user->id),
        ]));
    }

    /**
     * 续费：重置额度 + 换回可用密码（停用过的账号靠这一步恢复）。
     *
     * 上游没有「启用」开关，恢复可用 = 换一个新密码并把 proxy_limit 提回正常值，
     * 所以续费和停用是同一套动作的两个方向。
     *
     * @param  array{proxy_limit?:float, max_thread_count?:int}  $attrs
     */
    public function renewAccount(ProxyAccount $account, array $attrs = []): ProxyAccount
    {
        $password = $this->randomPassword();
        $proxyLimit = (float) ($attrs['proxy_limit'] ?? ($account->proxy_limit ?: 0));
        $maxThread = (int) ($attrs['max_thread_count'] ?? ($account->max_thread_count ?: 0));

        $account->provider->client()->updateSubuser($account->upstream_id, [
            'password' => $password,
            'proxy_limit' => $proxyLimit,
            'max_thread_count' => $maxThread,
        ]);

        $this->markProviderResult($account->provider, true, null);

        $account->forceFill([
            'password' => $password,
            // 同步上游导入的号可能没有本地 username，补一个可读的（只影响展示与交付）
            'username' => $account->username ?: ('u' . ($account->user_id ?: $account->id)),
            'proxy_limit' => $proxyLimit,
            'max_thread_count' => $maxThread,
            'status' => 'active',
            'last_synced_at' => now(),
        ])->save();

        return $account;
    }
}
