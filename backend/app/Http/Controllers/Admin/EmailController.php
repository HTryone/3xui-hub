<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Jobs\SendMailJob;
use App\Models\MailLog;
use App\Models\SiteConfig;
use App\Models\User;
use App\Services\MailNotifyService;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\Crypt;
use Illuminate\Support\Facades\Log;
use Illuminate\Support\Facades\Mail;

class EmailController extends Controller
{
    use ApiResponse;

    private const KEYS = [
        'smtp_host',
        'smtp_port',
        'smtp_username',
        'smtp_password',   // 加密存储
        'smtp_encryption',
        'smtp_from_name',
        'email_template',
        'register_email_verify', // 注册时启用邮箱验证

        // 用户端接口限流（见 App\Services\RateGuardService）：
        // 空值 = 用 config/panel.php 的默认值，0 = 该项不限
        'rate_code_interval',
        'rate_code_per_day',
        'rate_code_ip_hourly',
        'rate_verify_max_attempts',
        'rate_verify_lock_seconds',
        'rate_login_max_attempts',
        'rate_login_lock_seconds',
        'rate_register_ip_hourly',
        // 折扣码校验限流（rate_discount_*）不在这里：那 5 个参数归「优惠码管理 → 邀请机制设置」页，
        // 见 Admin\DiscountCodeController::inviteSettings()。
    ];

    /** 获取 SMTP 配置和邮件模板 */
    public function show(): \Illuminate\Http\JsonResponse
    {
        $config = SiteConfig::getMany(self::KEYS);

        // 解密密码
        if (!empty($config['smtp_password'])) {
            try {
                $config['smtp_password'] = Crypt::decryptString($config['smtp_password']);
            } catch (\Throwable) {
                $config['smtp_password'] = '';
            }
        }

        return $this->success($config);
    }

    /** 保存 SMTP 配置和邮件模板 */
    public function save(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'smtp_host'         => ['nullable', 'string', 'max:255'],
            'smtp_port'         => ['nullable', 'integer', 'min:1', 'max:65535'],
            'smtp_username'     => ['nullable', 'string', 'max:255'],
            'smtp_password'     => ['nullable', 'string', 'max:255'],
            'smtp_encryption'   => ['nullable', 'in:tls,ssl,none'],
            'smtp_from_name'    => ['nullable', 'string', 'max:100'],
            'email_template'    => ['nullable', 'string', 'max:65535'],
            'register_email_verify' => ['nullable', 'boolean'],

            // 限流：非负整数，0 = 不限（空值走 config/panel.php 默认）
            'rate_code_interval'       => ['nullable', 'integer', 'min:0'],
            'rate_code_per_day'        => ['nullable', 'integer', 'min:0'],
            'rate_code_ip_hourly'      => ['nullable', 'integer', 'min:0'],
            'rate_verify_max_attempts' => ['nullable', 'integer', 'min:0'],
            'rate_verify_lock_seconds' => ['nullable', 'integer', 'min:0'],
            'rate_login_max_attempts'  => ['nullable', 'integer', 'min:0'],
            'rate_login_lock_seconds'  => ['nullable', 'integer', 'min:0'],
            'rate_register_ip_hourly'  => ['nullable', 'integer', 'min:0'],
        ]);

        $updates = [];
        foreach ($data as $key => $value) {
            if ($key === 'smtp_password' && !empty($value)) {
                $value = Crypt::encryptString($value);
            }
            $updates[$key] = $value ?? '';
        }

        // 空值保护：SMTP 三件套（服务器/账号/授权码）一旦被空串写进去，
        // 面板就再也发不出邮件，而且没有任何提示——只能靠人工重新填授权码。
        // 触发路径：前端 load() 静默失败时表单还是初始空值，用户此时点保存，
        // 整份表单就会把库里的配置覆盖成空。这里挡下最后一道。
        // 语义：只提交非空值即视为「保持原样」；要真正清空某项请在数据库侧操作。
        $protected = ['smtp_host', 'smtp_username', 'smtp_password'];
        foreach ($protected as $key) {
            if (($updates[$key] ?? '') === '' && ($current = SiteConfig::getValue($key)) !== null && $current !== '') {
                unset($updates[$key]);
            }
        }

        // 发件地址不再单独存储：发信时固定取 smtp_username（见 SimpleMailerService）
        SiteConfig::setMany($updates);

        return $this->success(null, '保存成功');
    }

    /** 发送测试邮件 */
    public function test(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'to' => ['required', 'email'],
        ]);

        $config = SiteConfig::getMany(self::KEYS);

        if (empty($config['smtp_host'])) {
            return $this->error('请先配置 SMTP 服务器地址', 400);
        }

        // 解密密码
        $password = '';
        if (!empty($config['smtp_password'])) {
            try {
                $password = Crypt::decryptString($config['smtp_password']);
            } catch (\Throwable) {
            }
        }

        // 临时设置邮件驱动
        config([
            'mail.default'            => 'smtp',
            'mail.mailers.smtp.host'       => $config['smtp_host'],
            'mail.mailers.smtp.port'       => (int) ($config['smtp_port'] ?? 587),
            'mail.mailers.smtp.username'   => $config['smtp_username'] ?? null,
            'mail.mailers.smtp.password'   => $password,
            'mail.mailers.smtp.encryption' => ($config['smtp_encryption'] === 'none') ? null : $config['smtp_encryption'],
            'mail.mailers.smtp.local_domain' => 'localhost',
            'mail.mailers.smtp.auth_mode'  => 'login',
            'mail.mailers.smtp.timeout'    => 30,
            // 允许自签名证书（开发环境）
            'mail.mailers.smtp.stream_options' => [
                'ssl' => [
                    'verify_peer'       => false,
                    'verify_peer_name'  => false,
                    'allow_self_signed' => true,
                ],
            ],
        ]);

        // 强制刷新 mailer，否则会使用缓存的配置（log/null）
        app('mail.manager')->forgetMailers();

        // 发件地址 = 授权账号，二者必须一致（QQ 等邮箱会拒信，SMTP 501），不读 smtp_from_address
        $fromAddress = $config['smtp_username'] ?? '';
        $fromName    = $config['smtp_from_name']    ?? '';

        // 用模板发送，{{code}} 替换为 "TEST1234"
        $template = !empty($config['email_template']) ? $config['email_template'] : '<div style="padding:20px;font-family:sans-serif"><h2>注册验证码</h2><p style="font-size:24px;color:#2563eb;font-weight:bold">{{code}}</p><p style="color:#666">5分钟内有效，请勿泄露。</p></div>';
        $body = str_replace('{{code}}', 'TEST1234', $template);

        try {
            Mail::html($body, function ($message) use ($data, $fromAddress, $fromName) {
                $message->to($data['to'])
                    ->subject(!empty($fromName) ? $fromName : 'ControlHub');
                if ($fromAddress) {
                    $message->from($fromAddress, $fromName ?: null);
                }
            });
        } catch (\Throwable $e) {
            Log::error('Email test failed', ['error' => $e->getMessage()]);
            return $this->error('发送失败：' . $e->getMessage(), 500);
        }

        return $this->success(null, '测试邮件已发送');
    }

    // ============ 自动通知 ============

    /** 读取自动通知配置（全部开关默认关闭） */
    public function notifyConfig(): \Illuminate\Http\JsonResponse
    {
        return $this->success(MailNotifyService::allConfig());
    }

    /** 保存自动通知配置 */
    public function saveNotifyConfig(Request $request): \Illuminate\Http\JsonResponse
    {
        $scenes = array_keys(MailNotifyService::SCENES);
        $rules = [];
        foreach ($scenes as $scene) {
            $rules["notify_{$scene}_enabled"] = ['nullable', 'boolean'];
            $rules["notify_{$scene}_title"]   = ['nullable', 'string', 'max:255'];
            $rules["notify_{$scene}_body"]    = ['nullable', 'string', 'max:65535'];
            $rules["notify_{$scene}_to"]      = ['nullable', 'in:user,admin'];
        }
        $rules['notify_traffic_almost_threshold'] = ['nullable', 'integer', 'min:1', 'max:100'];
        $rules['notify_expiring_days']            = ['nullable', 'integer', 'min:0', 'max:365'];
        $rules['notify_admin_email']              = ['nullable', 'email'];

        $data = $request->validate($rules);

        $updates = [];
        foreach ($data as $key => $value) {
            if ($key === 'notify_admin_email') {
                $updates[$key] = $value ?? '';
                continue;
            }
            if (str_ends_with($key, '_enabled')) {
                // checkbox 传的是 0/1，存成 '1' / ''（未配置 = 关闭）
                $updates[$key] = $value ? '1' : '';
                continue;
            }
            $updates[$key] = $value ?? '';
        }

        SiteConfig::setMany($updates);

        return $this->success(MailNotifyService::allConfig(), '通知配置已保存');
    }

    // ============ 批量发信 ============

    /**
     * 群发 / 单发。
     * 全部走队列，页面不阻塞；限速开关与上限由管理员自己填（默认关闭限速）。
     */
    public function batchSend(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'subject'      => ['required', 'string', 'max:255'],
            'body'         => ['required', 'string', 'max:65535'],
            'mode'         => ['required', 'in:all,filter,single'],
            'plan_id'      => ['nullable', 'integer'],
            'user_status'  => ['nullable', 'in:normal,over,expired'],
            'single_query' => ['nullable', 'string', 'max:255'],
            'rate_limit_enabled' => ['nullable', 'boolean'],
            'rate_per_minute'    => ['nullable', 'integer', 'min:1', 'max:600'],
        ]);

        $config = SiteConfig::getMany(['smtp_host']);
        if (empty($config['smtp_host'])) {
            return $this->error('请先配置 SMTP 服务器地址', 400);
        }

        $query = $this->buildRecipientQuery($data);

        if ($data['mode'] === 'single') {
            $q = trim((string) ($data['single_query'] ?? ''));
            if ($q === '') {
                return $this->error('请填写要发给谁（邮箱或用户 ID）', 400);
            }
            // 单发：邮箱精确或用户 ID 精确，最多命中 1 人
            $user = User::where('email', $q)->orWhere('id', ctype_digit($q) ? (int) $q : 0)->first();
            if ($user === null) {
                return $this->error('没找到该用户', 404);
            }
            $recipients = collect([$user]);
        } else {
            $recipients = $query->get();
            if ($recipients->isEmpty()) {
                return $this->error('没有符合条件的用户', 400);
            }
        }

        $batchId = (int) now()->format('YmdHis');
        $subject = $data['subject'];
        $body    = $data['body'];

        // 限速：关闭时一次性全丢；开启时按「每分钟 N 封」错开 delay
        $limitEnabled = (bool) ($data['rate_limit_enabled'] ?? false);
        $perMinute    = max(1, (int) ($data['rate_per_minute'] ?? 30));

        // 用 values() 拿到 0..n-1 的连续下标：Eloquent 集合的 keys() 是主键，
        // 直接拿它算「第几分钟发」会得到错误的错开量。
        $recipients = $recipients->values();
        $total      = $recipients->count();

        foreach ($recipients as $i => $user) {
            $delaySeconds = $limitEnabled ? intdiv($i, $perMinute) * 60 : 0;

            $job = SendMailJob::dispatch(
                toEmail: (string) $user->email,
                subject: $subject,
                htmlBody: $this->renderForUser($body, $user),
                type: MailLog::TYPE_BATCH,
                userId: $user->id,
                batchId: $batchId,
            );

            if ($delaySeconds > 0) {
                $job->delay(now()->addSeconds($delaySeconds));
            }
        }

        return $this->success([
            'batch_id'     => $batchId,
            'total'        => $total,
            'rate_limited' => $limitEnabled ? $perMinute : null,
        ], "已加入发送队列，共 {$total} 封");
    }

    /** 按筛选条件构造收件人查询 */
    private function buildRecipientQuery(array $data): \Illuminate\Database\Eloquent\Builder
    {
        $q = User::with('plan');

        if (!empty($data['plan_id'])) {
            $q->where('plan_id', (int) $data['plan_id']);
        }

        switch ($data['user_status'] ?? '') {
            case 'over':
                // 超量：周期或总量任一超限
                $q->where(function ($w) {
                    $w->where(function ($x) {
                        $x->where('traffic_limit', '>', 0)->whereColumn('traffic_used', '>=', 'traffic_limit');
                    })->orWhere(function ($x) {
                        $x->where('monthly_traffic_limit', '>', 0)
                          ->whereColumn('monthly_traffic_used', '>=', 'monthly_traffic_limit');
                    });
                });
                break;
            case 'expired':
                $q->whereNotNull('expired_at')->where('expired_at', '<', now());
                break;
            case 'normal':
            default:
                // 正常：未禁用、未过期、未超量
                $q->where('enabled', true)
                  ->where(function ($x) {
                      $x->whereNull('expired_at')->orWhere('expired_at', '>', now());
                  })
                  ->where(function ($x) {
                      $x->where('traffic_limit', '<=', 0)
                        ->orWhereColumn('traffic_used', '<', 'traffic_limit');
                  });
                break;
        }

        return $q;
    }

    /** 为单个用户渲染正文变量 */
    private function renderForUser(string $body, User $user): string
    {
        $limit = (int) $user->traffic_limit;
        $used  = (int) $user->traffic_used;
        $vars = [
            '{{email}}'         => (string) $user->email,
            '{{user_id}}'       => (string) $user->id,
            '{{site_title}}'    => SiteConfig::getValue('site_title', 'ControlHub'),
            '{{used}}'          => $this->formatBytes($used),
            '{{limit}}'         => $this->formatBytes($limit),
            '{{percent}}'       => (string) ($limit > 0 ? round($used / $limit * 100, 1) : 0),
            '{{plan_name}}'     => $user->plan?->name ?? '无',
            '{{expire_date}}'   => $user->expired_at?->format('Y-m-d') ?? '—',
            '{{days_left}}'     => (string) ($user->expired_at ? (int) ceil(now()->diffInDays($user->expired_at, false)) : 0),
            '{{subscribe_url}}' => $user->token ? url('/api/sub/' . $user->token) : '',
        ];
        return strtr($body, $vars);
    }

    private function formatBytes(int $bytes): string
    {
        if ($bytes <= 0) return '0 B';
        $units = ['B', 'KB', 'MB', 'GB', 'TB'];
        $i = max(0, min((int) floor(log($bytes, 1024)), count($units) - 1));
        return round($bytes / pow(1024, $i), 2) . ' ' . $units[$i];
    }

    /** 发信日志（分页） */
    public function mailLogs(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'type'   => ['nullable', 'in:notify,batch,test'],
            'status' => ['nullable', 'in:sent,failed'],
            'page'   => ['nullable', 'integer', 'min:1'],
        ]);

        $query = MailLog::orderByDesc('id');
        if (!empty($data['type']))   $query->where('type', $data['type']);
        if (!empty($data['status'])) $query->where('status', $data['status']);

        $page   = (int) ($data['page'] ?? 1);
        $total  = (clone $query)->count();
        $rows   = $query->forPage($page, 50)->get();

        return response()->json([
            'code' => 0,
            'msg'  => 'ok',
            'data' => $rows->map(fn ($l) => [
                'id'         => $l->id,
                'type'       => $l->type,
                'status'     => $l->status,
                'to_email'   => $l->to_email,
                'to_user_id' => $l->to_user_id,
                'subject'    => $l->subject,
                'scene'      => $l->scene,
                'batch_id'   => $l->batch_id,
                'error'      => $l->error,
                'created_at' => $l->created_at?->toIso8601String(),
            ])->values()->all(),
            'pagination' => [
                'current_page' => $page,
                'last_page'    => max(1, (int) ceil($total / 50)),
                'total'        => $total,
                'per_page'     => 50,
            ],
        ], 200);
    }
}
