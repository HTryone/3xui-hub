<?php

namespace App\Services;

use App\Models\SiteConfig;
use App\Models\User;
use Illuminate\Support\Facades\Cache;

/**
 * 邮件自动通知配置与渲染。
 *
 * 设计要点：
 * - 所有通知项**默认关闭**（notify_*_enabled 未配置时视为false），管理员需显式开启。
 * - 配置存SiteConfig（key-value），无需改表结构。
 * - 渲染变量：{{email}} {{user_id}} {{site_title}} {{used}} {{limit}} {{percent}}
 *            {{plan_name}} {{expire_date}} {{days_left}} {{subscribe_url}}
 */
class MailNotifyService
{
    /** 通知项定义：key => [场景名, 默认标题, 默认正文, 默认收件人类型] */
    public const SCENES = [
        'traffic_exhausted' => [
            'label'  => '流量用尽',
            'title'  => '您的流量已用尽',
            'body'   => '<p>您好，</p><p>您的流量已用尽，流量已被暂停。请续费或重置流量后继续使用。</p>',
            'to'     => 'user',
        ],
        'traffic_almost' => [
            'label'  => '流量即将用尽',
            'title'  => '您的流量即将用尽',
            'body'   => '<p>您好，</p><p>您的流量已使用 {{percent}}%，请及时续费，避免服务中断。</p>',
            'to'     => 'user',
        ],
        'expiring' => [
            'label'  => '套餐即将到期',
            'title'  => '您的套餐即将到期',
            'body'   => '<p>您好，</p><p>您的套餐将于 {{expire_date}} 到期（剩余 {{days_left}} 天），请及时续费。</p>',
            'to'     => 'user',
        ],
        'expired' => [
            'label'  => '套餐已到期',
            'title'  => '您的套餐已到期',
            'body'   => '<p>您好，</p><p>您的套餐已于 {{expire_date}} 到期，流量已关闭。续费后可继续使用。</p>',
            'to'     => 'user',
        ],
    ];

    /** 配置键：通知开关 + 可调参数 */
    public const CONFIG_KEYS = [
        'notify_traffic_exhausted_enabled',
        'notify_traffic_exhausted_title',
        'notify_traffic_exhausted_body',
        'notify_traffic_exhausted_to',

        'notify_traffic_almost_enabled',
        'notify_traffic_almost_threshold',
        'notify_traffic_almost_title',
        'notify_traffic_almost_body',
        'notify_traffic_almost_to',

        'notify_expiring_enabled',
        'notify_expiring_days',
        'notify_expiring_title',
        'notify_expiring_body',
        'notify_expiring_to',

        'notify_expired_enabled',
        'notify_expired_title',
        'notify_expired_body',
        'notify_expired_to',

        'notify_admin_email',
    ];

    /** 某场景是否开启（未配置 → false，即默认关闭） */
    public static function isEnabled(string $scene): bool
    {
        return SiteConfig::getValue("notify_{$scene}_enabled") === '1';
    }

    /** 任意场景开启（调度条件用：四个通知全关时，邮件扫描任务整体跳过、不产生空转进程）。 */
    public static function anyEnabled(): bool
    {
        foreach (array_keys(self::SCENES) as $scene) {
            if (self::isEnabled($scene)) {
                return true;
            }
        }

        return false;
    }

    /** 读取某场景的通知配置（合并默认值） */
    public static function configFor(string $scene): array
    {
        $def = self::SCENES[$scene] ?? null;
        if ($def === null) {
            return [];
        }
        $cfg = SiteConfig::getMany([
            "notify_{$scene}_title",
            "notify_{$scene}_body",
            "notify_{$scene}_to",
        ]);

        return [
            'enabled' => self::isEnabled($scene),
            'label'   => $def['label'],
            'title'   => $cfg["notify_{$scene}_title"] !== '' ? $cfg["notify_{$scene}_title"] : $def['title'],
            'body'    => $cfg["notify_{$scene}_body"] !== '' ? $cfg["notify_{$scene}_body"] : $def['body'],
            'to'      => $cfg["notify_{$scene}_to"] !== '' ? $cfg["notify_{$scene}_to"] : $def['to'],
        ];
    }

    /** 全部通知项配置（供接口返回，扁平键与前端 loadNotify 的读取一致） */
    public static function allConfig(): array
    {
        $extra = SiteConfig::getMany([
            'notify_traffic_almost_threshold',
            'notify_expiring_days',
            'notify_admin_email',
        ]);

        $out = [];
        foreach (array_keys(self::SCENES) as $scene) {
            $cfg = self::configFor($scene);
            // enabled 存成 '1'/''（字符串）：前端用 === '1' 严格判断，布尔 true 会变成 false
            $out["notify_{$scene}_enabled"] = $cfg['enabled'] ? '1' : '';
            $out["notify_{$scene}_title"]   = $cfg['title'];
            $out["notify_{$scene}_body"]    = $cfg['body'];
            $out["notify_{$scene}_to"]      = $cfg['to'];
        }

        // 顶层扁平键：与前端 notify.notify_* 字段一一对应（原 _settings 折叠结构前端读不到）
        $out['notify_traffic_almost_threshold'] = (int) ($extra['notify_traffic_almost_threshold'] ?: 90);
        $out['notify_expiring_days']            = (int) ($extra['notify_expiring_days'] ?: 3);
        $out['notify_admin_email']              = $extra['notify_admin_email'];

        return $out;
    }

    /**
     * 渲染模板变量。
     *
     * @param string $scene 场景 key
     * @param User   $user  收件用户
     */
    public static function render(string $scene, User $user): array
    {
        $cfg = self::configFor($scene);
        if ($cfg === []) {
            return [];
        }

        $limit = (int) $user->traffic_limit;
        $used  = (int) $user->traffic_used;
        $percent = $limit > 0 ? round($used / $limit * 100, 1) : 0;

        $expireDate = $user->expired_at?->format('Y-m-d') ?? '—';
        $daysLeft   = $user->expired_at
            ? (int) ceil(now()->diffInDays($user->expired_at, false))
            : 0;

        $siteTitle = SiteConfig::getValue('site_title', 'ControlHub');
        $subUrl    = $user->token ? url('/api/sub/' . $user->token) : '';

        $vars = [
            '{{email}}'         => (string) $user->email,
            '{{user_id}}'       => (string) $user->id,
            '{{site_title}}'    => $siteTitle,
            '{{used}}'          => self::formatBytes($used),
            '{{limit}}'         => self::formatBytes($limit),
            '{{percent}}'       => (string) $percent,
            '{{plan_name}}'     => $user->plan?->name ?? '无',
            '{{expire_date}}'   => $expireDate,
            '{{days_left}}'     => (string) $daysLeft,
            '{{subscribe_url}}' => $subUrl,
        ];

        return [
            'subject' => strtr($cfg['title'], $vars),
            'body'    => strtr($cfg['body'], $vars),
            'to_type' => $cfg['to'],
        ];
    }

    /**
     * 恢复该用户的所有自动通知（重新购买 / 续费 / 重置流量后调用）。
     *
     * 原理：直接清掉「已发过」标记，之后任何场景再次触发条件都会重新提醒。
     * （早期实现是往 key 里拼「时代号」、靠递增让旧 key 永远读不到；但时代号按用户 ID 记、
     * 防重按邮箱记，同一邮箱的多个账号时代号不一致时 key 各不相同，防重会被击穿，
     * 故改为无状态 key + 显式清标记。）
     * 调用方：applyPlan（购买/续费/管理员改套餐）、renew（总量续费）、
     * resetTraffic（管理员重置）、completeResetOrder（付费重置订单）。
     */
    public static function restoreReminders(User $user): void
    {
        $store = self::claimStore();

        // 收件人是用户本人时按邮箱去重（whoOf 的口径），所以要把「当前邮箱」下的
        // 标记清掉；管理员通知按触发用户记（admin:{id}），一并清掉才完整。
        foreach (self::whoKeysForUser($user) as $key) {
            $store->forget($key);
        }
    }

    /** 该用户名下可能存在的全部防重 key（所有场景 × 邮箱维度 + admin 维度）。 */
    private static function whoKeysForUser(User $user): array
    {
        $keys = [];

        foreach (array_keys(self::SCENES) as $scene) {
            $keys[] = self::claimKeyFor($scene, self::whoOf('user', $user, (string) $user->email));
            $keys[] = self::claimKeyFor($scene, self::whoOf('admin', $user, ''));
        }

        return $keys;
    }

    /**
     * 该用户该场景是否还能发通知 —— **同一状态周期只发一次**，与自然月无关。
     *
     * 返回 true = 还没发过（继续发），false = 已发过（跳过）。
     * 所有自动通知的防重口径只有这一处（扫描类 3 个场景 + 流量用尽）。
     *
     * 与原月度去重的差异：不再按 Ym 月历格过期（跨月重发刷屏），
     * 而是跟着「用户状态周期」走 —— 只有重新购买/续费/重置（restoreReminders
     * 清标记）才会解锁下一封。用户一直不续费就永远只收一封。
     *
     * 去重维度跟着「收件人」走，不跟着被通知的用户：
     * - 发给用户本人 → 按邮箱（同一邮箱的多个账号只收一封，不把人刷屏；
     *   邮箱统一小写去空白，避免换大小写绕开计数。key 不带任何按用户变化的
     *   后缀，多个账号无论各自续没续费，同邮箱同场景都只有一个坑）
     * - 发给管理员 → 按触发用户（管理员的通知不能被先到的用户占掉坑，否则
     *   N 个用户到期只会收到 1 封，看起来像只有 1 个人有问题）
     *
     * **调用时机必须在「渲染 + 解析出收件地址」之后**：渲染早就落标记的话，
     * 管理员还没配收件邮箱时这一轮都发不出去（补上邮箱也要等恢复提醒才重发）。
     * 落标记而不是查 mail_logs，是因为日志要等队列真正发出才写，
     * 而扫描每 5 分钟一轮，队列没跑完时靠日志判断会把同一封发两遍。
     *
     * 标记存 **database store**（cache 表，key 主键 + insertOrIgnore）：file store
     * 一旦被 cache:clear / 误清 storage，全体标记归零、所有命中条件的用户会重收一封；
     * database store 与队列、会话同库同备份，生命周期和业务数据一致。
     * TTL 取 365 天（覆盖一次套餐周期绰绰有余）：标记过期=回到「未发过」，
     * 极端闲置一年多的用户最多多收一封，可接受。
     */
    public static function claimOnce(string $scene, User $user, string $toType, string $to): bool
    {
        $who = self::whoOf($toType, $user, $to);

        return self::claimStore()->add(self::claimKeyFor($scene, $who), 1, now()->addDays(365));
    }

    /**
     * 该场景 / 该收件人的防重 key（口径只有这一处）。
     *
     * 单独暴露出来是给「发送失败要补发」用：入队时把这个 key 一并交给 SendMailJob，
     * 真发失败就 forget 掉它，下一轮扫描（5 分钟一轮）会自动补发；
     * 否则 SMTP 抖一下这一封就永久丢了 —— 标记已落、触发条件仍在，却永远不会再发。
     */
    public static function claimKey(string $scene, User $user, string $toType, string $to): string
    {
        return self::claimKeyFor($scene, self::whoOf($toType, $user, $to));
    }

    private static function whoOf(string $toType, User $user, string $to): string
    {
        return $toType === 'admin'
            ? 'admin:' . $user->id
            : 'email:' . strtolower(trim($to));
    }

    private static function claimKeyFor(string $scene, string $who): string
    {
        return "mail_notify:{$scene}:{$who}";
    }

    /**
     * 防重标记专用 store。
     *
     * 显式走 database（config/cache.php 默认 CACHE_STORE=file）：file store 的
     * 数据只在 storage/framework/cache 里，cache:clear 或误删 storage 即全体标记归零。
     * database store 的 add() 底层是 insertOrIgnore（key 主键冲突即失败），天然原子。
     * config('cache.stores.database') 不存在时回落 default，保证自定义配置也能用。
     */
    private static function claimStore(): \Illuminate\Contracts\Cache\Repository
    {
        return Cache::store(config('cache.stores.database') ? 'database' : config('cache.default'));
    }

    /** 释放防重标记（发送临时失败时调用，让下一轮扫描补发）。 */
    public static function releaseClaim(?string $key): void
    {
        if ($key !== null && $key !== '') {
            self::claimStore()->forget($key);
        }
    }

    /** 字节转可读文本（用于 {{used}} {{limit}} 展示） */
    private static function formatBytes(int $bytes): string
    {
        if ($bytes <= 0) return '0 B';
        $units = ['B', 'KB', 'MB', 'GB', 'TB'];
        $i = (int) floor(log($bytes, 1024));
        $i = max(0, min($i, count($units) - 1));
        return round($bytes / pow(1024, $i), 2) . ' ' . $units[$i];
    }
}