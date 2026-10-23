<?php

namespace App\Console\Commands;

use App\Services\PhpRuntimeAuditor;
use Illuminate\Console\Command;

/**
 * PHP 运行时安全检查 / 应急缓解（php2shell：CVE-2026-109618 / CVE-2026-109619）。
 *
 * 已装机器的入口：`php artisan php:secure` 看结论，`--fix` 一键打缓解。
 * 应用层改不了这两个内核漏洞，本命令也不假装能修 —— 它把「你到底处于什么状态、
 * 下一步该做什么」讲清楚，并在运维决定先不升级时把已确认的利用路径堵上。
 *
 * 退出码语义（给 3hub update / 巡检脚本用）：
 *   0 = 运行时已含修复，或缓解项已全部生效（不再暴露于已确认的利用路径）
 *   1 = 仍暴露：版本未修且缓解项没生效 / 没敢动
 *   2 = 环境不支持自动改（容器、找不到 php.ini、权限不足）
 */
class PhpSecureCommand extends Command
{
    protected $signature = 'php:secure
        {--fix : 写入 ini 缓解项并重载 php-fpm（备份 + 校验 + 失败回滚）}
        {--revert : 撤销 ini 缓解块（升级完运行时之后把设置还回去）}
        {--json : 机器可读输出（后台 / 巡检用）}';

    protected $description = 'PHP 运行时安全检查（CVE-2026-109618/109619）：判定版本、按需写入或撤销 ini 缓解';

    public function handle(PhpRuntimeAuditor $auditor): int
    {
        $audit = $auditor->audit();
        $fix = (bool) $this->option('fix');
        $revert = (bool) $this->option('revert');

        if ($this->option('json')) {
            return $this->handleJson($auditor, $audit, $fix, $revert);
        }

        $this->render($audit);

        if ($revert) {
            return $this->revert($auditor, $audit);
        }

        if (! $audit['exposed']) {
            $this->info("运行时已含修复（{$audit['version']} ≥ {$audit['fixed_in']}），无需处理。");
            if ($audit['mitigated']) {
                $this->line('缓解块还留着（已无必要），要还回原设置：php artisan php:secure --revert');
            }

            return self::SUCCESS;
        }

        $this->warn("当前 PHP {$audit['version']} 在两个漏洞的修复版（{$audit['fixed_in']}）之前。");
        $this->line('SAPI：' . $audit['sapi'] . '（' . $this->sapiNote($audit['sapi']) . '）');

        if (! $fix) {
            $this->printMitigationHint($audit);
            $this->printUpgradeHint($auditor, $audit);

            return $audit['mitigated'] ? self::SUCCESS : self::FAILURE;
        }

        $result = $this->apply($auditor, $audit);
        if ($result !== self::SUCCESS) {
            return $result;
        }

        $this->newLine();
        $this->printUpgradeHint($auditor, $audit);

        return self::SUCCESS;
    }

    /** @param array<string,mixed> $audit */
    private function handleJson(PhpRuntimeAuditor $auditor, array $audit, bool $fix, bool $revert): int
    {
        $result = ['audit' => $audit, 'fix' => null, 'revert' => null];

        if ($revert) {
            $result['revert'] = $this->guard($auditor, $audit)
                ?? $auditor->removeMitigations((string) $audit['ini_file'], $auditor->detectService());
        } elseif ($fix && $audit['exposed']) {
            $result['fix'] = $this->guard($auditor, $audit)
                ?? $auditor->applyMitigations((string) $audit['ini_file'], $auditor->detectService());
        }

        $this->line((string) json_encode($result, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES));

        if ($revert) {
            return ($result['revert']['ok'] ?? false) === true ? self::SUCCESS : self::FAILURE;
        }

        if (! $audit['exposed'] || $audit['mitigated']) {
            return self::SUCCESS;
        }

        return ($result['fix']['ok'] ?? false) === true ? self::SUCCESS : self::FAILURE;
    }

    /**
     * 改 ini 之前的硬性检查：容器 / 找不到 ini / 不可写都不该动手。
     *
     * @param  array<string,mixed>  $audit
     * @return array{ok:false,backup:null,reason:string,reloaded:false,changed:false}|null
     */
    private function guard(PhpRuntimeAuditor $auditor, array $audit): ?array
    {
        if ($auditor->panelKind() === 'docker') {
            return ['ok' => false, 'backup' => null, 'reloaded' => false, 'changed' => false,
                'reason' => '容器环境：ini 缓解要写进镜像的 php.ini（或 compose 挂载），不要在这里改'];
        }

        if ($audit['ini_file'] === null) {
            return ['ok' => false, 'backup' => null, 'reloaded' => false, 'changed' => false,
                'reason' => '找不到已加载的 php.ini（php_ini_loaded_file() 为空）'];
        }

        if (! $audit['ini_writable']) {
            return ['ok' => false, 'backup' => null, 'reloaded' => false, 'changed' => false,
                'reason' => "{$audit['ini_file']} 不可写（当前用户 " . $this->currentUser() . '）'];
        }

        return null;
    }

    /**
     * 撤销 ini 缓解块。
     *
     * @param  array<string,mixed>  $audit
     */
    private function revert(PhpRuntimeAuditor $auditor, array $audit): int
    {
        if (($blocked = $this->guard($auditor, $audit)) !== null) {
            $this->error($blocked['reason']);

            return self::INVALID;
        }

        if ($audit['exposed']) {
            $this->warn('当前运行时仍在修复版（' . ($audit['fixed_in'] ?? '?') . '）之前：撤销后 '
                . 'session upload progress 这条已验证的利用路径会重新打开。');
        }

        $service = $auditor->detectService();
        $result = $auditor->removeMitigations((string) $audit['ini_file'], $service);

        if (! $result['ok']) {
            $this->error($result['reason']);

            return self::FAILURE;
        }

        if (! $result['changed']) {
            $this->info($result['reason'] . '。');

            return self::SUCCESS;
        }

        $this->info("已从 {$audit['ini_file']} 删除缓解块。");
        if ($result['reloaded']) {
            $this->line("已重载 {$service}。");
        }
        if ((string) ($result['reason'] ?? '') !== '') {
            $this->warn($result['reason']);
        }
        $this->line('原配置备份：' . $result['backup']);

        return self::SUCCESS;
    }

    /**
     * @param  array<string,mixed>  $audit
     * @return int 退出码
     */
    private function apply(PhpRuntimeAuditor $auditor, array $audit): int
    {
        if (($blocked = $this->guard($auditor, $audit)) !== null) {
            $this->error($blocked['reason']);
            if ($audit['ini_file'] !== null) {
                $this->line('手动追加到 FPM 的 php.ini 即可：' . $this->iniSnippet());
            }

            return self::INVALID;
        }

        $service = $auditor->detectService();
        $result = $auditor->applyMitigations($audit['ini_file'], $service);

        if (! $result['ok']) {
            $this->error($result['reason']);

            return self::FAILURE;
        }

        if (! $result['changed']) {
            $this->info($result['reason'] . '（无需重载）。');

            return self::SUCCESS;
        }

        $this->info("已在 {$audit['ini_file']} 写入缓解块。");
        if ($result['reloaded']) {
            $this->line("已重载 {$service}：worker 会读到新 ini。");
        }
        if ((string) ($result['reason'] ?? '') !== '') {
            // 「ini 写了但没重启成功 / 服务不 active」必须显式说出来，
            // 否则运维会以为缓解已经生效，实际 worker 还在跑旧 ini
            $this->warn($result['reason']);
        }
        $this->line('原配置备份：' . $result['backup']);
        $this->line("回滚：cp -a {$result['backup']} {$audit['ini_file']} && systemctl reload {$service}");
        $this->newLine();
        $this->warn('注意：这是缓解，不是修复。运行时仍需升级到 ' . $audit['fixed_in'] . '。');

        return self::SUCCESS;
    }

    /** @param array<string,mixed> $audit */
    private function render(array $audit): void
    {
        $this->line('PHP 版本 ：' . $audit['version'] . ' (' . $audit['sapi'] . ')');
        $this->line('修复版本 ：' . ($audit['fixed_in'] ?? '该分支官方已 EOL，无补丁'));
        $this->line('判定　　 ：' . ($audit['patched']
            ? '已含修复'
            : '受影响（php2shell CVE-2026-109618 / CVE-2026-109619，CVSS 9.2）'));
        $this->line('php.ini  ：' . ($audit['ini_file'] ?? '(未加载)')
            . ($audit['ini_file'] === null ? '' : ($audit['ini_writable'] ? '（可写）' : '（不可写）')));
        $this->newLine();

        $this->line('缓解项现状：');
        $rows = [];
        foreach ((array) $audit['mitigations'] as $key => $item) {
            $rows[] = [
                $key,
                $item['current'] === '' ? '(未设置)' : $item['current'],
                $item['wanted'],
                $item['ok'] ? '已生效' : '未生效',
            ];
        }
        $this->table(['指令', '当前', '期望', '状态'], $rows);
    }

    /** @param array<string,mixed> $audit */
    private function printMitigationHint(array $audit): void
    {
        if (! $audit['needs_mitigation']) {
            $this->info('缓解项已全部生效（advisory 已确认的利用路径已堵住），但仍需升级运行时。');

            return;
        }

        $this->line('未生效的缓解项会留下 advisory 已确认的利用路径。两种处理：');
        $this->line('  A. 打过缓解等升级窗口：php artisan php:secure --fix（自动备份 / 校验 / 失败回滚）');
        $this->line('  B. 手动在 php.ini（或 pool 的 php_admin_value）追加：');
        $this->line($this->iniSnippet());
    }

    private function printUpgradeHint(PhpRuntimeAuditor $auditor, array $audit): void
    {
        $this->line('升级（唯一完整修复，需在服务器上执行）：');

        switch ($auditor->panelKind()) {
            case 'baota':
                $this->line('  宝塔 → 软件商店 → PHP-' . ($audit['branch'] ?? '8.4') . ' → 设置 → 版本切换/更新到 '
                    . ($audit['fixed_in'] ?? '最新') . '，面板会重启 php-fpm；');
                $this->line('  然后重跑 php artisan php:secure 确认「已含修复」。');
                break;
            case 'docker':
                $this->line('  换镜像里 PHP 的版本（Dockerfile 的 FROM php:' . ($audit['branch'] ?? '8.4')
                    . ($audit['fixed_in'] ? '-fpm 或更高' : '') . ' → 8.4.27 起），然后重建容器。');
                break;
            default:
                $branch = str_replace('.', '', (string) ($audit['branch'] ?? '8.4'));
                $this->line('  Debian/Ubuntu：apt-get install --only-upgrade php' . ($audit['branch'] ?? '8.4')
                    . '-fpm && systemctl restart php' . ($audit['branch'] ?? '8.4') . '-fpm');
                $this->line('  Remi/RHEL    ：dnf update php' . $branch . '-php-fpm && systemctl restart php' . $branch . '-php-fpm');
                break;
        }
    }

    private function iniSnippet(): string
    {
        $lines = [];
        foreach ((array) config('panel.php_security.mitigations', []) as $key => $value) {
            $lines[] = '    ' . $key . ' = ' . $value;
        }

        return implode("\n", $lines);
    }

    private function sapiNote(string $sapi): string
    {
        return match (true) {
            $sapi === 'apache2handler' => 'mod_php 风险最高：109618 可在应用代码执行前打进来，建议优先升级',
            $sapi === 'fpm-fcgi', $sapi === 'cgi-fcgi' => 'FPM：109619 有已验证的 session upload progress 路径，务必关掉它',
            $sapi === 'cli' => '命令行 SAPI：不对外服务，本判定针对的是 Web worker',
            default => '未知 SAPI，按受影响处理',
        };
    }

    private function currentUser(): string
    {
        return function_exists('posix_getpwuid') && function_exists('posix_geteuid')
            ? (string) (posix_getpwuid(posix_geteuid())['name'] ?? 'unknown')
            : (string) (getenv('USER') ?: 'unknown');
    }
}
