<?php

namespace App\Services;

use Illuminate\Support\Facades\File;

/**
 * 备份文件里 .env 的处理。
 *
 * 只搬 APP_KEY 这一行：Node::$password / $apiKey、邮件与支付里的密码都是
 * Crypt::encryptString 加密存的，跨机迁移必须带上 APP_KEY，否则导入的节点凭据
 * 解不开（表现为节点测试失败、同步报错）。
 *
 * 但 .env 其余各行（DB 密码、QUEUE_CONNECTION、邮件配置）都是本机自己的，
 * 整份覆盖会把它们冲掉——历史上就发生过导入后 QUEUE_CONNECTION 变 sync、
 * 队列 Worker 空转的事故，所以这里只做逐行替换，不做整体覆盖。
 */
class BackupEnvService
{
    /** 导入预览里展示差异的服务器相关配置项。 */
    private const SERVER_KEYS = [
        'APP_NAME', 'APP_URL', 'APP_KEY', 'APP_DEBUG', 'APP_ENV',
        'DB_CONNECTION', 'DB_HOST', 'DB_PORT', 'DB_DATABASE',
        'SESSION_DRIVER', 'SESSION_DOMAIN',
        'MAIL_MAILER', 'MAIL_HOST', 'MAIL_PORT', 'MAIL_FROM_ADDRESS',
        'REDIS_HOST', 'REDIS_PORT',
    ];

    /** 预览里不回传明文、只标记「不同」的配置项。 */
    private const SENSITIVE_KEYS = ['APP_KEY'];

    /**
     * $envPath 为 null 时用本机 .env；测试传临时路径，避免动到真实 .env。
     */
    public function __construct(private readonly ?string $envPath = null)
    {
    }

    /**
     * 本机 .env 路径。
     */
    public function envPath(): string
    {
        return $this->envPath ?: base_path('.env');
    }

    /**
     * 导出用：备份 zip 里 .env 条目的内容，只有 APP_KEY 一行。
     * 本机没有 APP_KEY 时返回 null（zip 里不写 .env 条目）。
     */
    public function appKeyOnlyContents(): ?string
    {
        $appKey = $this->parseEnv($this->envPath())['APP_KEY'] ?? '';

        return $appKey === '' ? null : 'APP_KEY=' . $appKey . "\n";
    }

    /**
     * 导入用：把备份里的 .env 原文交给 restoreAppKeyFromEnvContents()。
     *
     * @param string|null $backupEnvContents 备份 zip 里 .env 条目的原文，null 表示没有该条目
     * @return string|null 需要展示给用户的提示；null 表示已正常恢复
     */
    public function restoreAppKeyFromEnvContents(?string $backupEnvContents): ?string
    {
        if ($backupEnvContents === null) {
            \Log::warning('Backup import: 备份里没有 .env，跳过 APP_KEY 恢复');
            return '备份里没有 .env，已跳过 APP_KEY 恢复（本机 .env 未改动）';
        }

        return $this->restoreAppKey($this->parseEnvContent($backupEnvContents)['APP_KEY'] ?? '');
    }

    /**
     * @param string $appKey 备份里的 APP_KEY，空字符串表示备份里没有
     * @return string|null 需要展示给用户的提示；null 表示已正常恢复
     */
    private function restoreAppKey(string $appKey): ?string
    {
        if ($appKey === '') {
            \Log::warning('Backup import: 备份里没有 APP_KEY，跳过 .env 恢复');
            return '备份里没有 APP_KEY，已跳过（本机 .env 未改动）';
        }

        $envPath = $this->envPath();
        if (!File::exists($envPath)) {
            \Log::warning('Backup import: 本机 .env 不存在，APP_KEY 未写入');
            return '本机 .env 不存在，APP_KEY 未写入';
        }

        // 覆盖前留一份，便于人工回退
        File::copy($envPath, $envPath . '.bak');
        $this->updateKey('APP_KEY', $appKey);
        \Log::info('Backup import: APP_KEY 已恢复（.env 其余行未改动）');

        return null;
    }

    /**
     * 更新本机 .env 中某个 key 的值：只改这一行，没有该行才追加，其余行不动。
     */
    public function updateKey(string $key, string $value): void
    {
        $envPath = $this->envPath();
        if (!File::exists($envPath)) return;

        $content = File::get($envPath);
        $pattern = '/^' . preg_quote($key, '/') . '=.*/m';
        $replacement = $key . '=' . $value;

        if (preg_match($pattern, $content)) {
            $content = preg_replace($pattern, $replacement, $content);
        } else {
            $content = rtrim($content) . "\n" . $replacement . "\n";
        }

        File::put($envPath, $content);
        // 只记 key 不记值：APP_KEY 这类敏感值不该进日志
        \Log::info("Backup import: {$key} updated");
    }

    /**
     * 预览用：备份 .env 与本机 .env 的差异。
     * 仅作展示——导入时只有 APP_KEY 会被替换，其余差异不会生效。
     */
    public function envDiff(array $backupEnv): array
    {
        $currentEnv = $this->parseEnv($this->envPath());

        $diff = [];
        foreach (self::SERVER_KEYS as $key) {
            $backupVal = $backupEnv[$key] ?? null;
            if ($backupVal === null || $backupVal === ($currentEnv[$key] ?? null)) continue;

            $masked = in_array($key, self::SENSITIVE_KEYS, true);
            $diff[] = [
                'key' => $key,
                'current' => $masked ? '不同' : ($currentEnv[$key] ?? '（未设置）'),
                'backup' => $masked ? '不同' : $backupVal,
            ];
        }

        return $diff;
    }

    /**
     * 读取备份 zip 里的 .env 原文；zip 里没有 .env 条目返回 null。
     */
    public function readBackupEnvContents(string $zipPath): ?string
    {
        $zip = new \ZipArchive();
        if ($zip->open($zipPath) !== true) return null;

        $index = $zip->locateName('.env');
        if ($index === false) {
            $zip->close();
            return null;
        }

        $content = (string) $zip->getFromIndex($index);
        $zip->close();

        return $content;
    }

    /**
     * 读取备份 zip 里的 .env（键值数组）；zip 里没有 .env 条目返回 null。
     */
    public function readBackupEnv(string $zipPath): ?array
    {
        $contents = $this->readBackupEnvContents($zipPath);

        return $contents === null ? null : $this->parseEnvContent($contents);
    }

    /**
     * 解析 .env 文件为键值数组。
     */
    public function parseEnv(string $path): array
    {
        return File::exists($path) ? $this->parseEnvContent(File::get($path)) : [];
    }

    /**
     * 解析 .env 文本为键值数组。
     */
    public function parseEnvContent(string $content): array
    {
        $result = [];
        foreach (preg_split('/\r\n|\n|\r/', $content) as $line) {
            $line = trim($line);
            if ($line === '' || $line[0] === '#') continue;
            $parts = explode('=', $line, 2);
            if (count($parts) === 2) {
                $result[trim($parts[0])] = trim($parts[1], " \t\n\r\0\x0B\"");
            }
        }

        return $result;
    }
}
