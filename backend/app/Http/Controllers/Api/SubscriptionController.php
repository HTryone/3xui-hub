<?php

namespace App\Http\Controllers\Api;

use App\Drivers\NodeDriverFactory;
use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Models\SiteConfig;
use App\Models\User;
use App\Services\BanService;
use App\Services\SubscriptionException;
use App\Services\SubscriptionService;
use App\Services\TrafficSyncService;
use Illuminate\Http\Request;
use Illuminate\Http\Response;

/**
 * 订阅接口：GET /api/sub/{token}
 * 支持格式：base64（默认）、clash、singbox
 * 参数：?clash=1 或 ?singbox=1
 */
class SubscriptionController extends Controller
{
    public function __construct(
        private SubscriptionService $service,
        private NodeDriverFactory $driverFactory,
        private TrafficSyncService $syncService,
        private BanService $banService,
    ) {}

    public function show(Request $request, string $token): Response
    {
        $user = User::where('token', $token)->first();

        if (!$user) {
            return $this->text('[404] 用户不存在', 404, '用户不存在');
        }

        // 加载用户数据（流量由定时任务同步，不在每次请求时同步）
        $user->load('plan');

        // 检查封禁状态
        if (!$user->enabled) {
            return $this->text('[403] 账号已禁用', 403, '账号已禁用');
        }

        // 确定格式
        $format = $this->resolveFormat($request);

        // 检查格式是否启用
        if (!$this->isFormatEnabled($format)) {
            return $this->text("[400] 不支持的订阅格式: {$format}", 400, '不支持的订阅格式');
        }

        try {
            $body = $this->service->generate($user, $format);
        } catch (SubscriptionException $e) {
            return $this->text("[{$e->codeValue}] {$e->getMessage()}", $e->codeValue, $e->getMessage());
        }

        // Subscription-Userinfo 头
        $upload = 0;
        $download = (int) $user->traffic_used;
        $total = (int) $user->traffic_limit;
        $expire = $user->expired_at ? $user->expired_at->timestamp : 0;

        // 根据格式设置 Content-Type
        $contentType = match ($format) {
            'clash' => 'text/yaml; charset=UTF-8',
            'singbox' => 'application/json; charset=UTF-8',
            default => 'text/plain; charset=UTF-8',
        };

        return response($body, 200)
            ->header('Content-Type', $contentType)
            ->header('Subscription-Userinfo', "upload={$upload}; download={$download}; total={$total}; expire={$expire}")
            ->header('X-CH-Code', '0')
            ->header('X-CH-Msg', 'ok');
    }

    /**
     * 确定请求的格式。
     */
    private function resolveFormat(Request $request): string
    {
        if ($request->has('clash') || $request->has('clash=1')) {
            return 'clash';
        }

        if ($request->has('singbox') || $request->has('singbox=1')) {
            return 'singbox';
        }

        return 'base64';
    }

    /**
     * 检查格式是否启用。
     */
    private function isFormatEnabled(string $format): bool
    {
        return match ($format) {
            'clash' => SiteConfig::getValue('sub_clash_enabled', '0') === '1',
            'singbox' => SiteConfig::getValue('sub_singbox_enabled', '0') === '1',
            default => true, // base64 始终启用
        };
    }

    /**
     * 同步用户在所有节点上的流量。
     */
    private function syncUserTraffic(User $user): void
    {
        Node::where('enabled', true)->each(function (Node $node) use ($user) {
            try {
                $driver = $this->driverFactory->make($node);
                $traffic = $driver->getClientTraffic($user->clientEmail());
                $this->syncService->syncUserNode($user, $node, $traffic);
            } catch (\Throwable $e) {
                // 节点离线，跳过
            }
        });

        // 检查封禁
        $fresh = $user->fresh();
        $fresh->load('plan');
        $this->banService->checkAfterSync($fresh);
    }

    private function text(string $body, int $code = 0, string $msg = 'ok'): Response
    {
        return response($body, 200)
            ->header('Content-Type', 'text/plain; charset=UTF-8')
            ->header('X-CH-Code', (string) $code)
            ->header('X-CH-Msg', $msg);
    }
}
