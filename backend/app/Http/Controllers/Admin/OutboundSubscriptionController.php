<?php

namespace App\Http\Controllers\Admin;

use App\Drivers\Xray\OutboundSubscriptionService;
use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Models\XrayOutboundSubscription;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Illuminate\Validation\ValidationException;

/**
 * Admin xray 节点「出站订阅」（服务器池）管理
 * （/admin-api/nodes/{node}/xray-outbound-subscriptions）。
 *
 * - CRUD + 立即刷新（同步拉取一次回显结果）
 * - 拉取前 SSRF 校验（仅 http/https + 非内网），非法 URL 直接拒绝
 * - 停用/删除/内容变化 → bump 该节点 config_version
 */
class OutboundSubscriptionController extends Controller
{
    use ApiResponse;

    public function __construct(
        private readonly OutboundSubscriptionService $service,
    ) {}

    /** GET /admin-api/nodes/{node}/xray-outbound-subscriptions */
    public function index(Node $node): JsonResponse
    {
        $this->ensureXray($node);

        $subs = $node->xrayOutboundSubscriptions()->orderBy('priority')->orderBy('id')->get();

        return $this->success([
            'items' => $subs->map(fn (XrayOutboundSubscription $s) => $this->present($s))->values()->all(),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** POST /admin-api/nodes/{node}/xray-outbound-subscriptions */
    public function store(Request $request, Node $node): JsonResponse
    {
        $this->ensureXray($node);
        $data = $this->validatePayload($request);

        try {
            $url = $this->service->sanitizeUrl((string) $data['url']);
        } catch (\InvalidArgumentException $e) {
            throw ValidationException::withMessages(['url' => $e->getMessage()]);
        }

        $sub = new XrayOutboundSubscription();
        $sub->node_id = $node->id;
        $this->fillFromPayload($sub, $data, $url);
        $sub->last_status = 'pending';
        // 新订阅排在同类末尾
        $sub->priority = (int) $node->xrayOutboundSubscriptions()->max('priority') + 1;
        $sub->save();

        // 新建后立刻拉一次（失败保留 pending 状态 + 记原因，不阻塞创建）
        $this->service->refresh($sub);

        return $this->success([
            'subscription' => $this->present($sub->fresh()),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** PUT /admin-api/nodes/{node}/xray-outbound-subscriptions/{subscription} */
    public function update(Request $request, Node $node, XrayOutboundSubscription $subscription): JsonResponse
    {
        $this->ensureXray($node);
        $this->ensureOwned($node, $subscription);

        $data = $this->validatePayload($request);

        try {
            $url = $this->service->sanitizeUrl((string) $data['url']);
        } catch (\InvalidArgumentException $e) {
            throw ValidationException::withMessages(['url' => $e->getMessage()]);
        }

        $urlChanged = $url !== (string) $subscription->url;
        $this->fillFromPayload($subscription, $data, $url);
        $subscription->save();

        // URL 变了立刻重拉（否则缓存还是旧源的数据）
        if ($urlChanged) {
            $this->service->refresh($subscription);
        }

        $node->bumpXrayConfigVersion();

        return $this->success([
            'subscription' => $this->present($subscription->fresh()),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** DELETE /admin-api/nodes/{node}/xray-outbound-subscriptions/{subscription} */
    public function destroy(Node $node, XrayOutboundSubscription $subscription): JsonResponse
    {
        $this->ensureXray($node);
        $this->ensureOwned($node, $subscription);

        $subscription->delete();
        $node->bumpXrayConfigVersion();

        return $this->success(['config_version' => $node->xrayConfigVersion()]);
    }

    /** POST /admin-api/nodes/{node}/xray-outbound-subscriptions/{subscription}/refresh */
    public function refresh(Node $node, XrayOutboundSubscription $subscription): JsonResponse
    {
        $this->ensureXray($node);
        $this->ensureOwned($node, $subscription);

        $this->service->refresh($subscription);
        $subscription->refresh();

        return $this->success(
            $this->present($subscription),
            $subscription->last_status === 'success' ? '拉取成功' : '拉取失败（已保留上次成功缓存）',
        );
    }

    // ── 内部 ─────────────────────────────────────────────

    private function ensureXray(Node $node): void
    {
        if (! $node->isXray()) {
            abort(422, '仅 xray 节点支持该操作');
        }
    }

    private function ensureOwned(Node $node, XrayOutboundSubscription $sub): void
    {
        if ((int) $sub->node_id !== (int) $node->id) {
            abort(404);
        }
    }

    private function validatePayload(Request $request): array
    {
        return $request->validate([
            'remark' => ['nullable', 'string', 'max:128'],
            'url' => ['required', 'string', 'max:2048'],
            'tag_prefix' => ['nullable', 'string', 'max:64', 'regex:/^[A-Za-z0-9_.-]*$/'],
            'update_interval' => ['nullable', 'integer', 'min:60', 'max:86400'],
            'prepend' => ['nullable', 'boolean'],
            'priority' => ['nullable', 'integer', 'min:0', 'max:65535'],
            'enabled' => ['nullable', 'boolean'],
        ]);
    }

    private function fillFromPayload(XrayOutboundSubscription $sub, array $data, string $url): void
    {
        $sub->url = $url;
        $sub->remark = (string) ($data['remark'] ?? '') !== '' ? (string) $data['remark'] : null;
        $sub->tag_prefix = (string) ($data['tag_prefix'] ?? '') !== '' ? (string) $data['tag_prefix'] : null;
        $sub->update_interval = (int) ($data['update_interval'] ?? 600) ?: 600;
        if (array_key_exists('prepend', $data)) {
            $sub->prepend = (bool) $data['prepend'];
        }
        if (array_key_exists('priority', $data)) {
            $sub->priority = (int) $data['priority'];
        }
        if (array_key_exists('enabled', $data)) {
            $sub->enabled = (bool) $data['enabled'];
        }
    }

    /** 展示结构（url 含 token 属敏感，仅管理端返回）。 */
    private function present(XrayOutboundSubscription $s): array
    {
        $url = (string) $s->url;
        $masked = mb_strlen($url) > 24
            ? mb_substr($url, 0, 12) . '…' . mb_substr($url, -8)
            : $url;

        return [
            'id' => $s->id,
            'remark' => $s->remark,
            'url' => $url,
            'url_masked' => $masked,
            'tag_prefix' => $s->tag_prefix,
            'update_interval' => (int) $s->update_interval,
            'prepend' => (bool) $s->prepend,
            'priority' => (int) $s->priority,
            'enabled' => (bool) $s->enabled,
            'node_count' => (int) $s->node_count,
            'outbound_count' => count($s->outbounds()),
            'last_fetched_at' => $s->last_fetched_at?->toIso8601String(),
            'last_status' => $s->last_status,
            'last_error' => $s->last_error,
        ];
    }
}
