<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\DiscountCode;
use App\Models\DiscountCodeFolder;
use App\Models\SiteConfig;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\DB;

/**
 * 管理员优惠码 + 邀请机制配置接口。
 */
class DiscountCodeController extends Controller
{
    use ApiResponse;

    /** 邀请机制可配置字段（走 SiteConfig） */
    private const INVITE_KEYS = [
        'invite_discount',
        'invite_refresh_days',
        'invite_enabled',
        'redeem_discount',

        // 优惠码校验限流（见 App\Services\RateGuardService 的 rate_discount_*）：
        // 与其它 rate_* 同一个约定 —— 空值 = 用 config/panel.php ratelimit 段默认值，0 = 该项不限。
        // 这类参数归「优惠码」业务，故放在本页而不是邮箱配置页。
        'rate_discount_per_minute',
        'rate_discount_fail_per_day',
        'rate_discount_ip_fail_hourly',
        'rate_discount_max_attempts',
        'rate_discount_lock_seconds',
    ];

    /** 上面 4 个邀请字段之外的限流项，校验规则与读值方式单独走一套。 */
    private const RATE_KEYS = [
        'rate_discount_per_minute',
        'rate_discount_fail_per_day',
        'rate_discount_ip_fail_hourly',
        'rate_discount_max_attempts',
        'rate_discount_lock_seconds',
    ];

    /**
     * 优惠码列表（可按 source/status/user/folder 筛选，分页）。
     *
     * 分组口径（prompts/discount-folders-20261023）：
     * - 未传 folder：list 只出未归档码（folder_id = null），文件夹内的码一律不进 list
     * - 传 folder=id：list 只出该文件夹内的码
     * - folders 始终返回全部文件夹，code_count 按当前筛选条件计数；
     *   有筛选条件且该文件夹内有命中项时标 matched（前端据此提示「搜索命中在文件夹里」）
     */
    public function index(Request $request): \Illuminate\Http\JsonResponse
    {
        $request->validate([
            'source' => ['sometimes', 'nullable', 'in:invite,redeem,admin'],
            'status' => ['sometimes', 'nullable', 'in:active,used_up,expired,refreshed'],
            'user' => ['sometimes', 'nullable', 'string', 'max:255'],
            'folder' => ['sometimes', 'nullable', 'integer', 'exists:discount_code_folders,id'],
            'page' => ['sometimes', 'integer', 'min:1'],
            'per_page' => ['sometimes', 'integer', 'min:1', 'max:100'],
        ], [
            // 新参数给中文文案：默认的英文 "The selected folder is invalid." 出现在
            // 「文件夹刚被别的会话删了」这种真实场景里，用户看不懂
            'folder.exists' => '文件夹不存在',
        ]);

        $perPage = (int) $request->input('per_page', 20);

        // 同一套筛选条件要给 list 和 folders 计数各用一次，抽成闭包避免两处走偏
        $applyFilters = function ($query) use ($request): void {
            if ($request->filled('source')) {
                $query->where('source', $request->input('source'));
            }
            if ($request->filled('status')) {
                $query->where('status', $request->input('status'));
            }
            if ($request->filled('user')) {
                // 按持有用户邮箱模糊搜。whereHas 走子查询，user_id 为 null 的管理员码
                // 天然不匹配（而不是报错或误伤），所以这里不用额外判空。
                $keyword = $request->input('user');
                $query->whereHas('user', fn ($q) => $q->where('email', 'like', "%{$keyword}%"));
            }
        };

        $folderId = $request->filled('folder') ? (int) $request->input('folder') : null;

        $query = DiscountCode::query()->with(['user:id,email', 'plan:id,name']);
        $applyFilters($query);
        if ($folderId !== null) {
            $query->where('folder_id', $folderId);
        } else {
            $query->whereNull('folder_id');
        }

        $total = (clone $query)->count();
        $items = $query->orderByDesc('id')
            ->forPage((int) $request->input('page', 1), $perPage)
            ->get()
            ->map(fn (DiscountCode $c) => [
                'id' => $c->id,
                'code' => $c->code,
                'source' => $c->source,
                'user_id' => $c->user_id,
                'user_email' => $c->user->email ?? null,
                'discount' => (float) $c->discount,
                'max_uses' => $c->max_uses,
                'max_uses_per_user' => $c->max_uses_per_user,
                'used_count' => $c->used_count,
                'expires_at' => $c->expires_at?->toDateTimeString(),
                'note' => $c->note,
                'status' => $c->status,
                'plan_id' => $c->plan_id,
                'plan_name' => $c->plan->name ?? null,
                'folder_id' => $c->folder_id,
                'created_at' => $c->created_at?->toDateTimeString(),
            ]);

        // 有没有生效的筛选条件：决定 folders 里的 matched 是否有意义
        // （没筛选时「命中」是废话，一律不标，前端就不会满屏「命中」）
        $filtering = $request->filled('source') || $request->filled('status') || $request->filled('user');

        $folders = DiscountCodeFolder::query()
            ->orderBy('name')
            ->orderBy('id')
            ->withCount(['codes' => function ($q) use ($applyFilters): void {
                $applyFilters($q);
            }])
            ->get()
            ->map(fn (DiscountCodeFolder $f) => [
                'id' => $f->id,
                'name' => $f->name,
                'code_count' => (int) $f->codes_count,
                'matched' => $filtering && (int) $f->codes_count > 0,
            ]);

        return $this->success([
            'list' => $items,
            'folders' => $folders,
            'total' => $total,
            'page' => (int) $request->input('page', 1),
            'per_page' => $perPage,
        ]);
    }

    /**
     * 生成优惠码（支持批量生成 N 个）。
     *
     * prefix：码前缀，默认 PRO，1~10 位字母数字（入库前转大写），码 = 前缀 + 8 位随机。
     * folder_id / folder_name：归档到哪个文件夹，folder_name 不存在则新建；两者都给时以 id 为准。
     */
    public function store(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'discount' => ['required', 'numeric', 'gt:0', 'lte:1'],
            'max_uses' => ['nullable', 'integer', 'min:1'],
            'max_uses_per_user' => ['nullable', 'integer', 'min:1'],
            'expires_at' => ['nullable', 'date'],
            'note' => ['nullable', 'string', 'max:255'],
            'plan_id' => ['nullable', 'integer', 'exists:plans,id'],
            'count' => ['nullable', 'integer', 'min:1', 'max:500'],
            // 大小写都收（小写会在下面转大写），非法字符/超长直接 422
            'prefix' => ['sometimes', 'nullable', 'string', 'regex:/^[A-Z0-9]{1,10}$/i'],
            'folder_id' => ['nullable', 'integer', 'exists:discount_code_folders,id'],
            'folder_name' => ['sometimes', 'nullable', 'string', 'max:60'],
        ]);

        $prefix = strtoupper(trim((string) ($data['prefix'] ?? '')));
        if ($prefix === '') {
            $prefix = 'PRO';
        }

        $folderId = $this->resolveFolderId($data);

        $count = (int) ($data['count'] ?? 1);
        $codes = DiscountCode::generateCodes($prefix, $count);

        if ($codes) {
            // 批量插入：500 个从「逐条 create」的 4 秒降到百毫秒级。
            // insert() 不走模型事件/casts，所以 expires_at 要自己转成 DB 格式、时间戳手动填。
            $now = now();
            $expiresAt = isset($data['expires_at'])
                ? \Illuminate\Support\Carbon::parse($data['expires_at'])->toDateTimeString()
                : null;

            $rows = array_map(fn (string $code) => [
                'code' => $code,
                'source' => DiscountCode::SOURCE_ADMIN,
                'user_id' => null,
                'discount' => $data['discount'],
                'max_uses' => $data['max_uses'] ?? 1,
                'used_count' => 0,
                'max_uses_per_user' => $data['max_uses_per_user'] ?? 1,
                'expires_at' => $expiresAt,
                'note' => $data['note'] ?? null,
                'status' => DiscountCode::STATUS_ACTIVE,
                'plan_id' => $data['plan_id'] ?? null,
                'folder_id' => $folderId,
                'created_at' => $now,
                'updated_at' => $now,
            ], $codes);

            // 整批一个事务：分批 insert 中途失败时不能留下「生成了 300 个、接口报错」的半批数据
            DB::transaction(function () use ($rows): void {
                foreach (array_chunk($rows, 100) as $chunk) {
                    DiscountCode::insert($chunk);
                }
            });
        }

        return $this->success([
            'count' => count($codes),
            'codes' => $codes,
            'folder_id' => $folderId,
        ], '生成成功');
    }

    /**
     * 修改：折扣 / 有效期 / 总次数 / 说明文字 / 状态。
     */
    public function update(Request $request, string $code): \Illuminate\Http\JsonResponse
    {
        $discountCode = $this->resolveCode($code);
        if (! $discountCode) {
            return $this->error('优惠码不存在', 404);
        }

        $data = $request->validate([
            'discount' => ['sometimes', 'numeric', 'gt:0', 'lte:1'],
            'max_uses' => ['sometimes', 'integer', 'min:1'],
            'max_uses_per_user' => ['sometimes', 'integer', 'min:1'],
            'expires_at' => ['sometimes', 'nullable', 'date'],
            'note' => ['sometimes', 'nullable', 'string', 'max:255'],
            'status' => ['sometimes', 'in:active,used_up,expired,refreshed'],
            'plan_id' => ['sometimes', 'nullable', 'integer', 'exists:plans,id'],
        ]);

        // 次数下限保护：used_count 是已经发生的真实消耗，把上限改到它之下会让数据自相矛盾
        // （显示「已用 2 次 / 上限 1 次」）。要降上限得先删掉多出来的订单，不能靠改字段掩盖。
        if (array_key_exists('max_uses', $data) && $data['max_uses'] < $discountCode->used_count) {
            return $this->error("使用次数上限不能小于已使用次数（当前已用 {$discountCode->used_count} 次）", 400);
        }

        $discountCode->fill($data)->save();

        return $this->success(null, '保存成功');
    }

    /**
     * 删除。
     */
    public function destroy(string $code): \Illuminate\Http\JsonResponse
    {
        $discountCode = $this->resolveCode($code);
        if (! $discountCode) {
            return $this->error('优惠码不存在', 404);
        }

        $discountCode->delete();
        return $this->success(null, '已删除');
    }

    /**
     * POST /admin-api/discount-codes/batch-delete
     * body: { ids: number[] } —— 一次删多个优惠码（与入站批量删除同一套口径）。
     *
     * - 逐条删，单条失败不影响其余，按 id 回报 failed
     * - 不存在的 id 归入 failed(not_found)，不静默吞
     */
    public function batchDestroy(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            // 上限与生成接口保持一致（500）：生成 500 个后要能一次全选删掉
            'ids' => ['required', 'array', 'min:1', 'max:500'],
            'ids.*' => ['integer', 'min:1'],
        ]);

        $ids = array_values(array_unique(array_map('intval', $data['ids'])));
        $targets = DiscountCode::whereIn('id', $ids)->get()->keyBy('id');

        $deleted = [];
        $failed = [];

        foreach ($ids as $id) {
            $code = $targets->get($id);
            if (! $code) {
                $failed[] = ['id' => $id, 'reason' => 'not_found'];
                continue;
            }
            try {
                $code->delete();
                $deleted[] = $id;
            } catch (\Throwable $e) {
                $failed[] = ['id' => $id, 'reason' => $e->getMessage()];
            }
        }

        return $this->success(['deleted' => $deleted, 'failed' => $failed]);
    }

    /**
     * 读邀请机制配置（含优惠码校验限流）。
     *
     * 限流 5 项刻意回「生效值」而不是原始 SiteConfig 值：没配过时回 config/panel.php 的默认值，
     * 页面输入框就直接显示当前真正在用的阈值，不会出现「框是空的、实际却在按 10 次/分拦」的错觉。
     */
    public function inviteSettings(): \Illuminate\Http\JsonResponse
    {
        $data = [
            'invite_discount' => SiteConfig::getValue('invite_discount', '0.90'),
            'invite_refresh_days' => SiteConfig::getValue('invite_refresh_days', '30'),
            'invite_enabled' => SiteConfig::getValue('invite_enabled', '1'),
            'redeem_discount' => SiteConfig::getValue('redeem_discount', '0.90'),
        ];

        foreach (self::RATE_KEYS as $key) {
            $stored = SiteConfig::getValue($key, '');
            $data[$key] = ($stored === '')
                ? (string) config('panel.ratelimit.' . $key, 0)
                : $stored;
        }

        return $this->success($data);
    }

    /**
     * 改邀请机制配置（折扣、刷新周期、开关、优惠码校验限流）。
     */
    public function updateInviteSettings(Request $request): \Illuminate\Http\JsonResponse
    {
        $rules = [
            'invite_discount' => ['sometimes', 'numeric', 'gt:0', 'lte:1'],
            'invite_refresh_days' => ['sometimes', 'integer', 'min:1'],
            'invite_enabled' => ['sometimes', 'boolean'],
            'redeem_discount' => ['sometimes', 'numeric', 'gt:0', 'lte:1'],
        ];

        // 限流项：非负整数，0 = 不限，空值 = 回落到 config/panel.php 默认值
        foreach (self::RATE_KEYS as $key) {
            $rules[$key] = ['sometimes', 'nullable', 'integer', 'min:0'];
        }

        $data = $request->validate($rules);

        foreach (self::INVITE_KEYS as $key) {
            if (! array_key_exists($key, $data)) {
                continue;
            }
            // 开关统一存 '1' / '0'，其它值按字符串存
            // （限流项传 null 时 (string) 后是空串，正好等于「没配过、用 panel 默认值」）
            $value = $key === 'invite_enabled'
                ? ($request->boolean('invite_enabled') ? '1' : '0')
                : (string) $data[$key];

            SiteConfig::setValue($key, $value);
        }

        return $this->success(null, '保存成功');
    }

    /**
     * 路由参数 {code} 既支持主键 id 也支持码本身。
     */
    private function resolveCode(string $code): ?DiscountCode
    {
        return DiscountCode::where('id', $code)
            ->orWhere('code', strtoupper(trim($code)))
            ->first();
    }

    /**
     * 生成时的归档目标：folder_id 优先；否则按 folder_name 找，不存在就建一个。
     * 两者都没给 → null（未归档），与老码一致。
     */
    private function resolveFolderId(array $data): ?int
    {
        if (! empty($data['folder_id'])) {
            return (int) $data['folder_id'];
        }

        $name = trim((string) ($data['folder_name'] ?? ''));
        if ($name === '') {
            return null;
        }

        // firstOrCreate 的并发窗口交给 name 不做唯一约束的既有约定：
        // 极端情况下可能建出同名文件夹，属于展示层无伤，不为了它加唯一索引打挂归档操作
        return (int) DiscountCodeFolder::firstOrCreate(['name' => $name])->id;
    }
}
