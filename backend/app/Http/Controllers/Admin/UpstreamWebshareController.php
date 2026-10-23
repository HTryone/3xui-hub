<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\ProxyAccount;
use App\Models\ProxyProvider;
use App\Services\Webshare\ProxyProviderService;
use App\Services\Webshare\WebshareException;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;

/**
 * Admin 代理上游 · Webshare（/admin-api/upstream/webshare）。
 *
 * 站长配自己的 Webshare 账号（可多个），给平台用户开独立子用户。
 * 只做后端接口，页面骨架另行接入。
 *
 * 安全口径：api_token 与子用户 password 都加密存储，接口一律不回明文；
 * provider 只回 has_token，账号只回 username + 用量。
 */
class UpstreamWebshareController extends Controller
{
    use ApiResponse;

    public function __construct(
        private ProxyProviderService $service,
    ) {}

    // ===== provider（站长账号）=====

    /** GET /admin-api/upstream/webshare/providers */
    public function index(): JsonResponse
    {
        $providers = ProxyProvider::orderByDesc('id')->get();

        return $this->success([
            'items' => $providers->map(fn (ProxyProvider $p) => $this->service->providerSummary($p))->values(),
        ]);
    }

    /** POST /admin-api/upstream/webshare/providers */
    public function storeProvider(Request $request): JsonResponse
    {
        $data = $request->validate([
            'name' => ['required', 'string', 'max:64'],
            'api_token' => ['required', 'string', 'max:512'],
            'enabled' => ['sometimes', 'boolean'],
        ]);

        $provider = ProxyProvider::create([
            'name' => $data['name'],
            'driver' => 'webshare',
            'api_token' => $data['api_token'],
            'enabled' => $data['enabled'] ?? true,
            'last_status' => 'pending',
        ]);

        return $this->success($this->service->providerSummary($provider), '已创建');
    }

    /** PUT /admin-api/upstream/webshare/providers/{provider} */
    public function updateProvider(Request $request, ProxyProvider $provider): JsonResponse
    {
        $data = $request->validate([
            'name' => ['sometimes', 'string', 'max:64'],
            'api_token' => ['sometimes', 'string', 'max:512'],
            'enabled' => ['sometimes', 'boolean'],
        ]);

        $provider->forceFill($data)->save();

        return $this->success($this->service->providerSummary($provider->fresh()), '已更新');
    }

    /** DELETE /admin-api/upstream/webshare/providers/{provider} */
    public function destroyProvider(ProxyProvider $provider): JsonResponse
    {
        // 子账号随 provider 级联删除（只删本地记录，不动 Webshare 侧子用户）
        $provider->delete();

        return $this->success(null, '已删除');
    }

    /** POST /admin-api/upstream/webshare/providers/{provider}/test */
    public function testProvider(ProxyProvider $provider): JsonResponse
    {
        try {
            $result = $this->service->test($provider);
        } catch (WebshareException $e) {
            return $this->error($e->getMessage(), 502);
        }

        return $this->success($result, '连接正常');
    }

    /** GET /admin-api/upstream/webshare/providers/{provider}/plans */
    public function plans(ProxyProvider $provider): JsonResponse
    {
        try {
            $plans = $this->service->syncPlans($provider);
        } catch (WebshareException $e) {
            return $this->error($e->getMessage(), 502);
        }

        return $this->success(['items' => $plans]);
    }

    // ===== account（子用户）=====

    /** GET /admin-api/upstream/webshare/providers/{provider}/accounts */
    public function accounts(ProxyProvider $provider): JsonResponse
    {
        $accounts = $provider->accounts()->orderByDesc('id')->get();

        return $this->success([
            'items' => $accounts->map(fn (ProxyAccount $a) => $this->service->accountSummary($a))->values(),
        ]);
    }

    /** POST /admin-api/upstream/webshare/providers/{provider}/accounts */
    public function storeAccount(Request $request, ProxyProvider $provider): JsonResponse
    {
        $data = $request->validate([
            'label' => ['required', 'string', 'max:64'],
            // GB；0 = 不限量
            'proxy_limit' => ['sometimes', 'numeric', 'min:0'],
            'max_thread_count' => ['sometimes', 'integer', 'min:0'],
            'user_id' => ['sometimes', 'nullable', 'integer', 'exists:users,id'],
            'proxy_countries' => ['sometimes', 'nullable', 'array'],
        ]);

        try {
            $account = $this->service->createAccount($provider, $data);
        } catch (WebshareException $e) {
            return $this->error($e->getMessage(), 502);
        }

        // 初始密码只在「建号」这一次响应里回给管理员：上游同样只在创建响应里给一次
        // （GET /subuser/{id}/ 不返回凭据）。不回的话管理员拿不到凭据，开号等于白开。
        // 列表 / 详情 / 用量回采一律不带密码。
        return $this->success(
            array_merge($this->service->accountSummary($account), [
                'initial_password' => $account->password,
            ]),
            '已创建'
        );
    }

    /** POST /admin-api/upstream/webshare/accounts/{account}/disable */
    public function disableAccount(ProxyAccount $account): JsonResponse
    {
        try {
            $this->service->disableAccount($account);
        } catch (WebshareException $e) {
            return $this->error($e->getMessage(), 502);
        }

        // 停用 = 上游密码已被改成随机串，旧凭据作废。新密码同样只在这**一次**响应里给
        // （管理员要交付给用户才能恢复使用），列表接口永不返回。
        return $this->success(
            array_merge($this->service->accountSummary($account->fresh()), [
                'new_password' => $account->fresh()->password,
            ]),
            '已停用'
        );
    }

    /** DELETE /admin-api/upstream/webshare/accounts/{account} */
    public function destroyAccount(ProxyAccount $account): JsonResponse
    {
        try {
            $this->service->deleteAccount($account);
        } catch (WebshareException $e) {
            return $this->error($e->getMessage(), 502);
        }

        return $this->success(null, '已删除');
    }

    /** POST /admin-api/upstream/webshare/accounts/{account}/usage —— 单个回采。 */
    public function pullUsage(ProxyAccount $account): JsonResponse
    {
        try {
            $this->service->pullUsage($account);
        } catch (WebshareException $e) {
            return $this->error($e->getMessage(), 502);
        }

        return $this->success($this->service->accountSummary($account->fresh()));
    }

    /** POST /admin-api/upstream/webshare/providers/{provider}/usage —— 批量回采。 */
    public function pullUsageBatch(ProxyProvider $provider): JsonResponse
    {
        $result = $this->service->pullUsageBatch(
            $provider->accounts()->where('status', 'active')->get()
        );

        return $this->success($result);
    }

    /**
     * POST /admin-api/upstream/webshare/accounts/{account}/credential
     * 读取本地保存的子用户凭据（管理员显式点击才调）。
     *
     * 上游没有“读密码”接口（只读详情不返凭据），这里读的是建号/停用时
     * 我们自己加密存下来的副本。从上游同步来的记录没有这份副本 → 404，
     * 必须先停用重置才会生成新密码。
     *
     * 用 POST 而不是 GET：明文凭据不该进浏览器缓存/历史/Referer。
     */
    public function credential(ProxyAccount $account): JsonResponse
    {
        $password = (string) $account->password;

        if ($password === '') {
            return $this->error('本地没有该子用户的密码（从上游同步来的记录不带凭据），需要停用重置后才能拿到新密码', 404);
        }

        return $this->success([
            'id' => $account->id,
            'label' => $account->label,
            'username' => (string) $account->username,
            'password' => $password,
        ]);
    }

    /**
     * GET /admin-api/upstream/webshare/providers/{provider}/proxies?plan_id=
     * 代理 IP 列表（只回 IP/端口/国家，不回主账号代理凭据）。
     */
    public function proxies(Request $request, ProxyProvider $provider): JsonResponse
    {
        $planId = $request->filled('plan_id') ? (int) $request->input('plan_id') : null;

        try {
            $result = $this->service->listProxies($provider, $planId);
        } catch (WebshareException $e) {
            return $this->error($e->getMessage(), 502);
        }

        return $this->success($result);
    }

    /**
     * POST /admin-api/upstream/webshare/providers/{provider}/accounts/sync
     * 把上游已有子用户同步到本地（站长在 Webshare 后台直接建过号时用）。
     */
    public function syncAccounts(ProxyProvider $provider): JsonResponse
    {
        try {
            $result = $this->service->syncAccounts($provider);
        } catch (WebshareException $e) {
            return $this->error($e->getMessage(), 502);
        }

        return $this->success($result, '已同步');
    }
}
