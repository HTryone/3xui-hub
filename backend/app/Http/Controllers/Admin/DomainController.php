<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\Domain;
use App\Services\DomainService;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\DB;

/**
 * Admin 多域名管理。
 * GET /admin-api/domains
 * POST /admin-api/domains
 * POST /admin-api/domains/{id}/primary
 * POST /admin-api/domains/{id}/apply
 * POST /admin-api/domains/{id}/renew
 * DELETE /admin-api/domains/{id}
 */
class DomainController extends Controller
{
    use ApiResponse;

    public function __construct(private DomainService $domains) {}

    public function index(): \Illuminate\Http\JsonResponse
    {
        $domains = Domain::where('enabled', 1)
            ->orderByDesc('is_primary')
            ->orderBy('domain')
            ->get();

        return $this->success($domains->map(fn (Domain $d) => $this->present($d))->values());
    }

    public function store(Request $request): \Illuminate\Http\JsonResponse
    {
        $domain = strtolower(trim((string) $request->input('domain', '')));

        if ($domain === '' || mb_strlen($domain) > 190) {
            return $this->error('请填写域名（不超过 190 个字符）', 1);
        }

        // 合法主机名校验：字母/数字/连字符，至少一段
        if (!preg_match('/^(?=^.{3,253}$)([a-z0-9]([a-z0-9\-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9\-]{0,61}[a-z0-9])?$/', $domain)) {
            return $this->error('域名格式不正确', 1);
        }

        if (Domain::where('domain', $domain)->exists()) {
            return $this->error('域名已存在', 1);
        }

        // 表内尚无 enabled 行时，新行直接为主域
        $isPrimary = !Domain::where('enabled', 1)->exists();

        $record = Domain::create([
            'domain' => $domain,
            'is_primary' => $isPrimary,
            'ssl_status' => 'pending',
        ]);

        return $this->success($this->present($record), '创建成功');
    }

    public function setPrimary(Request $request, int $id): \Illuminate\Http\JsonResponse
    {
        $target = Domain::find($id);
        if (!$target || !$target->enabled) {
            return $this->error('域名不存在', 1);
        }

        return DB::transaction(function () use ($target): \Illuminate\Http\JsonResponse {
            Domain::where('id', '!=', $target->id)
                ->update(['is_primary' => false]);

            $target->is_primary = true;
            $target->save();

            return $this->success($this->present($target->fresh()), '已设为主域');
        });
    }

    /** 一键应用：签发/续签证书并按 domains 表重建 nginx。立即返回 task_id。 */
    public function apply(Request $request, int $id): \Illuminate\Http\JsonResponse
    {
        return $this->submitAction($id, 'apply', '已提交应用');
    }

    /** 强制续签该域证书并重装。立即返回 task_id。 */
    public function renew(Request $request, int $id): \Illuminate\Http\JsonResponse
    {
        return $this->submitAction($id, 'renew', '已提交续签');
    }

    public function destroy(Request $request, int $id): \Illuminate\Http\JsonResponse
    {
        $target = Domain::find($id);
        if (!$target || !$target->enabled) {
            return $this->error('域名不存在', 1);
        }

        $enabledCount = Domain::where('enabled', 1)->count();

        // 唯一 enabled 行（必然是主域）不能删除
        if ($target->is_primary && $enabledCount === 1) {
            return $this->error('主域名不能删除', 1);
        }

        $deletedId = $target->id;
        $deletedName = $target->domain;
        $wasPrimary = (bool) $target->is_primary;
        $target->delete();

        // 删的是主域：剩余 enabled 行中字典序第一行提为主域
        if ($wasPrimary) {
            $next = Domain::where('enabled', 1)->orderBy('domain')->first();
            if ($next) {
                $next->is_primary = true;
                $next->save();
            }
        }

        // 删除后按剩余 enabled 域名重建 nginx，清掉该域 80/443 块
        $this->domains->submit('remove', $deletedId, $deletedName);

        return $this->success(null, '已删除');
    }

    private function submitAction(int $id, string $action, string $msg): \Illuminate\Http\JsonResponse
    {
        $target = Domain::find($id);
        if (!$target || !$target->enabled) {
            return $this->error('域名不存在', 1);
        }

        $task = $this->domains->submit($action, $target->id, $target->domain);

        return $this->success(['task_id' => $task->id], $msg);
    }

    private function present(Domain $d): array
    {
        return [
            'id' => $d->id,
            'domain' => $d->domain,
            'is_primary' => (bool) $d->is_primary,
            'enabled' => (bool) $d->enabled,
            'ssl_status' => $d->ssl_status,
            'cert_expires_at' => $d->cert_expires_at?->toIso8601String(),
            'last_error' => $d->last_error,
            'created_at' => $d->created_at?->toIso8601String(),
        ];
    }
}
