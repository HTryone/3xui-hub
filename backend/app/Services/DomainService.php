<?php

namespace App\Services;

use App\Jobs\ApplyDomainJob;
use App\Models\AsyncTask;
use App\Models\Domain;
use Carbon\Carbon;
use Illuminate\Support\Facades\Log;

/**
 * 多域名 SSL/Nginx 应用。
 *
 * 生产环境通过受限 sudoers 调 /usr/local/bin/3hub-domain（apply|renew|remove|status），
 * 解析 stdout JSON 后回写 domains 表（ssl_status / cert_expires_at / last_error）。
 *
 * 本地开发没有该助手脚本时：不抛异常，直接标 ssl_status=failed + last_error 提示未安装。
 */
class DomainService
{
    public const HELPER = '/usr/local/bin/3hub-domain';

    public const HELPER_MISSING_MSG = '3hub-domain 助手未安装（本地开发环境）';

    public const DOMAIN_RE = '/^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$/';

    public function __construct(private AsyncTaskService $tasks) {}

    /**
     * 提交域名异步操作（apply|renew|remove），返回 async_tasks 行（调用方拿 task_id）。
     * apply/renew 先把该行 ssl_status 置 pending，前端据此显示「配置中」。
     */
    public function submit(string $action, int $domainId, string $domainName): AsyncTask
    {
        if ($action === 'apply' || $action === 'renew') {
            Domain::whereKey($domainId)->update([
                'ssl_status' => 'pending',
                'last_error' => null,
                'updated_at' => now(),
            ]);
        }

        $task = $this->tasks->create(
            'domain_action',
            'domain',
            $domainId,
            1,
            ['action' => $action, 'domain' => $domainName],
            3,
            ['domain:'.$domainId],
        );

        $this->tasks->dispatchAfterCommit($task);

        return $task;
    }

    /** 签发/续签证书 → 装证 → 按表重建 nginx conf → reload。 */
    public function apply(Domain $domain): array
    {
        return $this->run('apply', $domain);
    }

    /** --force-reissue 强制续签 → 重装 → reload。 */
    public function renew(Domain $domain): array
    {
        return $this->run('renew', $domain);
    }

    /** 按剩余 enabled 域名重建 conf（该域已从表中删除，块随之消失）。 */
    public function remove(Domain $domain): array
    {
        return $this->run('remove', $domain);
    }

    /** 读 openssl 证书到期时间。 */
    public function status(Domain $domain): array
    {
        return $this->run('status', $domain);
    }

    private function run(string $action, Domain $domain): array
    {
        $name = strtolower(trim((string) $domain->domain));

        if ($name === '' || !preg_match(self::DOMAIN_RE, $name)) {
            return $this->finish($domain, 'failed', null, '域名格式不正确', $action);
        }

        // 本地开发 / 助手未装：标失败并回写，不抛异常
        if (!is_file(self::HELPER)) {
            return $this->finish($domain, 'failed', null, self::HELPER_MISSING_MSG, $action);
        }

        // 助手的人类日志（含 acme.sh 真实报错）一律走 stderr，不能丢。
        // 注意：不能 2>&1（日志会混进 stdout 破坏 JSON 解析），改存临时文件，失败时透出。
        $errFile = tempnam(sys_get_temp_dir(), '3hubdom_');
        $cmd = sprintf(
            'sudo -n %s %s %s 2>%s',
            escapeshellarg(self::HELPER),
            escapeshellarg($action),
            escapeshellarg($name),
            escapeshellarg((string) $errFile)
        );

        $lines = [];
        $exit = 0;
        @exec($cmd, $lines, $exit);

        $stderr = '';
        if ($errFile) {
            $stderr = trim((string) @file_get_contents($errFile));
            @unlink($errFile);
        }
        if ($stderr !== '') {
            $stderr = mb_substr($stderr, -800);
        }

        $json = json_decode(implode("\n", $lines), true);
        if (!is_array($json)) {
            return $this->finish($domain, 'failed', null, $this->withStderr("3hub-domain 输出无法解析（exit={$exit}）", $stderr), $action);
        }

        $ok = (bool) ($json['ok'] ?? false);
        $ssl = (string) ($json['ssl_status'] ?? ($ok ? 'ok' : 'failed'));
        if (!in_array($ssl, ['none', 'pending', 'ok', 'expiring', 'failed'], true)) {
            $ssl = $ok ? 'ok' : 'failed';
        }
        $expires = $json['cert_expires_at'] ?? null;
        $error = isset($json['error']) && $json['error'] !== null ? (string) $json['error'] : null;

        if (!$ok) {
            $error = $this->withStderr($error ?: '3hub-domain 执行失败', $stderr);
        }

        return $this->finish($domain, $ssl, $expires, $error, $action);
    }

    /** 失败时把助手 stderr 尾部拼进错误信息，便于在页面上直接看到真实原因。 */
    private function withStderr(string $message, string $stderr): string
    {
        return $stderr === '' ? $message : $message."\n".$stderr;
    }

    /**
     * 回写 domains 表，并把失败写进日志（页面上弹出的错误，日志里必须能查到）。
     * 域名行已删除（remove 后）时跳过回写，绝不插回。
     */
    private function finish(Domain $domain, string $sslStatus, mixed $expiresAt, ?string $error, ?string $action = null): array
    {
        if ($domain->exists) {
            $domain->forceFill([
                'ssl_status' => $sslStatus,
                'cert_expires_at' => $expiresAt ? Carbon::parse($expiresAt) : null,
                'last_error' => $error,
            ])->save();
        }

        if ($error !== null && $error !== '') {
            Log::error('域名操作失败', [
                'domain' => (string) $domain->domain,
                'action' => (string) ($action ?? ''),
                'ssl_status' => $sslStatus,
                'error' => $error,
            ]);
        }

        return [
            'ok' => $sslStatus === 'ok' || $sslStatus === 'expiring',
            'ssl_status' => $sslStatus,
            'cert_expires_at' => $expiresAt,
            'error' => $error,
        ];
    }
}
