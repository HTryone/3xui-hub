<?php

namespace App\Services;

/**
 * PHP 运行时安全基线（php2shell：CVE-2026-109618 / CVE-2026-109619）。
 *
 * 两个漏洞都在 PHP 内核，跟我们自己的代码无关：
 *   - CVE-2026-109618：OOM 时用 longjmp 提前退出，请求内存可能只初始化了一半，
 *     shutdown 阶段把它当有效数据读 —— mod_php 上可做到「应用代码跑之前就 RCE」。
 *   - CVE-2026-109619：一个请求留下的全局状态（error/session/object/extension）没被复位，
 *     同一 worker 的下一个请求在正常初始化之前读到已释放的内存（UAF）。
 *
 * 所以修复分两层，本类只负责判定与第二层：
 *   1.（唯一完整修复）把 PHP 升到 8.4.27 / 8.3.36 / 8.2.35 / 8.5.12 —— 只能由运维在机器上做；
 *   2.（缓解）断掉 advisory 已确认的利用路径：session upload progress 这个 consumer
 *      （FPM 上唯一被验证过的路径）、8.5 的 fatal_error_backtraces consumer。
 *
 * 注意 109619 的 consumer 跑在「正常请求初始化之前」，所以鉴权拦不住、error_reporting
 * 和 @ 也压不住 —— 缓解项必须落在 php.ini / pool 的 php_admin_value，`.user.ini` 与
 * ini_set() 都太晚。
 */
class PhpRuntimeAuditor
{
    /** ini 缓解块标记：重复执行时整块替换，不会越写越多。 */
    public const MARK_BEGIN = '; >>> controlhub php runtime security (CVE-2026-109618/109619) >>>';

    public const MARK_END = '; <<< controlhub php runtime security <<<';

    /** 可注入的命令执行器 fn(string $cmd): array{0:int,1:string} —— 测试用，生产走 shell。 */
    private $runner;

    public function __construct(?callable $runner = null)
    {
        $this->runner = $runner;
    }

    /**
     * 版本是否已含两个漏洞的修复。
     *
     * 发行版版本串带后缀（8.4.27-1+ubuntu24.04.1、8.5.12-1.el9、8.3.36~deb12u1），
     * 取前三段数字比较；不在 patched_branches 表内的分支（8.1 及更早）官方根本不发补丁，
     * 一律判受影响（有发行版 backport 的走 8.4/8.3 分支即可）。
     */
    public function isPatched(string $version): bool
    {
        $fixed = $this->fixedIn($version);
        if ($fixed === null) {
            return false;
        }

        $triple = $this->triple($version);
        $cmp = version_compare($triple, $fixed, '>=');
        if (! $cmp) {
            return false;
        }

        // 补丁位相同但带预发布标记（8.4.27~pre1、8.4.27-rc1）不是修复版本身
        if (version_compare($triple, $fixed, '=') && preg_match('/(~|\brc\d*|alpha|beta|\bpre\d*|\bdev)/i', trim($version))) {
            return false;
        }

        return true;
    }

    /** 该版本所在分支的修复版本号；分支已 EOL（官方无补丁）或版本串无法解析时返回 null。 */
    public function fixedIn(string $version): ?string
    {
        $branch = $this->branch($version);
        if ($branch === null) {
            return null;
        }

        $table = (array) config('panel.php_security.patched_branches', []);

        return isset($table[$branch]) ? (string) $table[$branch] : null;
    }

    /** "8.4.27-1+ubuntu24.04.1" → "8.4"；解析不出来返回 null。 */
    public function branch(string $version): ?string
    {
        if (! preg_match('/^(\d+)\.(\d+)(?:\.\d+)?/', trim($version), $m)) {
            return null;
        }

        return $m[1] . '.' . $m[2];
    }

    /** "8.4.27-1+ubuntu" → "8.4.27"（只取前三段数字，够 version_compare 用）。 */
    private function triple(string $version): string
    {
        if (! preg_match('/^(\d+)\.(\d+)\.(\d+)/', trim($version), $m)) {
            // 缺补丁位（如 "8.4"）时补 0，避免调用方拿到 false 却以为是「已修」
            return trim($version) . '.0';
        }

        return $m[1] . '.' . $m[2] . '.' . $m[3];
    }

    /** @return array<string,string> 需要写入的 ini 缓解项（键 = 指令，值 = 期望值） */
    public function mitigations(): array
    {
        return (array) config('panel.php_security.mitigations', []);
    }

    /**
     * 体检：版本 / 分支 / SAPI / ini 路径 / 每条缓解项的实际生效值。
     *
     * 缓解项的 current 取自 ini_get() —— 在生产里就是当前 worker 真实生效的值
     * （含 pool 的 php_admin_value 覆盖），这是唯一可信的口径。
     *
     * @return array{
     *     version:string,branch:?string,sapi:string,patched:bool,fixed_in:?string,
     *     exposed:bool,ini_file:?string,ini_writable:bool,
     *     mitigations:array<string,array{current:string,wanted:string,ok:bool}>,
     *     mitigated:bool,needs_mitigation:bool
     * }
     */
    public function audit(?string $version = null, ?string $sapi = null): array
    {
        $version = $version ?? PHP_VERSION;
        $sapi = $sapi ?? PHP_SAPI;

        $iniFile = function_exists('php_ini_loaded_file') ? (php_ini_loaded_file() ?: null) : null;

        $mitigations = [];
        $allOk = true;
        foreach ($this->mitigations() as $key => $wanted) {
            $current = (string) ini_get($key);
            $ok = $this->valueMatches($current, (string) $wanted);
            $mitigations[$key] = ['current' => $current, 'wanted' => (string) $wanted, 'ok' => $ok];
            $allOk = $allOk && $ok;
        }

        $patched = $this->isPatched($version);

        return [
            'version'          => $version,
            'branch'           => $this->branch($version),
            'sapi'             => $sapi,
            'patched'          => $patched,
            'fixed_in'         => $this->fixedIn($version),
            'exposed'          => ! $patched,
            'ini_file'         => $iniFile,
            'ini_writable'     => $iniFile !== null && is_writable($iniFile),
            'mitigations'      => $mitigations,
            'mitigated'        => $mitigations !== [] && $allOk,
            // 只在「运行时未修 + 还有缓解项没生效」时才建议动手
            'needs_mitigation' => ! $patched && ! $allOk,
        ];
    }

    /**
     * ini 值比较：ini_get 的布尔是 "1"/""，配置里写的是 "On"/"0"，统一归一后再比。
     */
    public function valueMatches(string $current, string $wanted): bool
    {
        return $this->normalize($current) === $this->normalize($wanted);
    }

    private function normalize(string $value): string
    {
        $value = strtolower(trim($value));

        if (in_array($value, ['1', 'on', 'yes', 'true'], true)) {
            return 'on';
        }

        if (in_array($value, ['', '0', 'off', 'no', 'false', 'none'], true)) {
            return 'off';
        }

        return $value;
    }

    /** 重载 php-fpm 用的服务名：配置优先，其次复用 fpm.targets 探测结果，最后兜底 php-fpm。 */
    public function detectService(): string
    {
        $configured = trim((string) config('panel.php_security.service', ''));
        if ($configured !== '') {
            return $configured;
        }

        foreach ((array) config('panel.fpm.targets', []) as $target) {
            $conf = (string) ($target['conf'] ?? '');
            if ($conf !== '' && is_file($conf)) {
                return (string) ($target['service'] ?? 'php-fpm');
            }
        }

        return 'php-fpm';
    }

    /** 面板类型：只用于把「怎么升 PHP」讲对（宝塔走面板，Debian/Remi 走包管理器）。 */
    public function panelKind(): string
    {
        if (is_dir('/www/server/panel')) {
            return 'baota';
        }

        if (file_exists('/.dockerenv')) {
            return 'docker';
        }

        return 'linux';
    }

    /**
     * 写入 ini 缓解块并重载 php-fpm。任一步失败即回滚，盘上不留坏配置。
     *
     * 幂等：块已存在且内容一致时不动文件、不重复重载。
     *
     * @return array{ok:bool,backup:?string,reason:string,reloaded:bool,changed:bool}
     */
    public function applyMitigations(string $iniFile, ?string $service = null): array
    {
        $original = $this->readIni($iniFile);
        if (is_array($original)) {
            return $original;
        }

        $block = $this->buildBlock();
        if (str_contains($original, $block)) {
            // 内容完全一致：已经写过了，不需要再动文件 / 再重载一次
            return ['ok' => true, 'backup' => null, 'reason' => 'ini 缓解块已存在且内容一致，未改动', 'reloaded' => false, 'changed' => false];
        }

        $rest = $this->stripBlock($original);
        $trailing = str_ends_with($rest, "\n") || $rest === '' ? '' : "\n";

        return $this->commit($iniFile, $original, $rest . $trailing . $block, $service);
    }

    /**
     * 撤销 ini 缓解块 —— 升级完运行时之后用，把 upload progress 之类还回原样。
     *
     * @return array{ok:bool,backup:?string,reason:string,reloaded:bool,changed:bool}
     */
    public function removeMitigations(string $iniFile, ?string $service = null): array
    {
        $original = $this->readIni($iniFile);
        if (is_array($original)) {
            return $original;
        }

        if (! str_contains($original, self::MARK_BEGIN)) {
            return ['ok' => true, 'backup' => null, 'reason' => '没有找到缓解块，无需撤销', 'reloaded' => false, 'changed' => false];
        }

        return $this->commit($iniFile, $original, $this->stripBlock($original), $service);
    }

    /** 读 ini；读不到时直接返回 fail 结构（调用方用 is_array 区分，不必自己再拼错误返回）。 */
    private function readIni(string $iniFile): string|array
    {
        if (! is_file($iniFile)) {
            return $this->fail("找不到 php.ini：{$iniFile}");
        }

        $content = @file_get_contents($iniFile);
        if ($content === false) {
            return $this->fail("读取 {$iniFile} 失败（权限？）");
        }

        return $content;
    }

    /**
     * 落盘 → 校验 → 重载 → 确认服务活着；任何一步不对就回到原样。
     *
     * @return array{ok:bool,backup:?string,reason:string,reloaded:bool,changed:bool}
     */
    private function commit(string $iniFile, string $original, string $updated, ?string $service): array
    {
        $backup = $iniFile . '.bak.' . date('YmdHis');
        if (! @copy($iniFile, $backup)) {
            return $this->fail("备份到 {$backup} 失败，未改动 {$iniFile}");
        }

        if (@file_put_contents($iniFile, $updated) === false) {
            @unlink($backup);

            return $this->fail("写入 {$iniFile} 失败（权限？），未改动配置");
        }

        $service = $service ?? $this->detectService();

        // 语法校验：php -c <ini> 能干净跑完一个探针脚本，才说明 ini 没写坏。
        [$code, $out] = $this->validateIni($iniFile);
        if ($code !== 0) {
            $this->restore($iniFile, $original, $backup);

            return $this->fail('php.ini 校验失败，已回滚：' . $this->firstLine($out));
        }

        // 没有服务管理器（开发机 / 非 systemd）：ini 是对的，不该回滚，
        // 但要明确告诉人「没帮你重启，worker 还在跑旧 ini」—— 否则等于骗自己。
        if (! $this->hasServiceManager()) {
            return [
                'ok' => true, 'backup' => $backup, 'changed' => true, 'reloaded' => false,
                'reason' => 'ini 已改，但机器上没有 systemctl/service，未重启 php-fpm：请手动重启使其生效',
            ];
        }

        // reload(SIGUSR2) 会让 master 重新读 php.ini 且不丢请求；失败再退到 restart
        [$reloadCode, $reloadOut] = $this->run(
            'systemctl reload ' . escapeshellarg($service) . ' 2>/dev/null'
            . ' || systemctl restart ' . escapeshellarg($service) . ' 2>/dev/null'
            . ' || service ' . escapeshellarg($service) . ' restart'
        );
        if ($reloadCode !== 0) {
            $this->restore($iniFile, $original, $backup);

            return $this->fail("重载 {$service} 失败，已回滚：{$this->firstLine($reloadOut)}");
        }

        // reload 成功 ≠ 服务活着（ini 写坏会让 master 起不来，面板就整站打不开）。
        // 起不来也不回滚 ini —— 回滚解决不了已经挂掉的服务，但必须让人立刻知道。
        [$activeCode, $activeOut] = $this->run('systemctl is-active ' . escapeshellarg($service) . ' 2>/dev/null');
        if ($activeCode !== 0 && trim($activeOut) !== 'active') {
            $state = trim($activeOut) === '' ? '查不到状态' : trim($activeOut);

            return [
                'ok' => true, 'backup' => $backup, 'changed' => true, 'reloaded' => false,
                'reason' => "已写入并重载，但 {$service} 当前不是 active（{$state}）：请立刻 systemctl status "
                    . $service . ' 确认面板可访问',
            ];
        }

        return ['ok' => true, 'backup' => $backup, 'reason' => '', 'reloaded' => true, 'changed' => true];
    }

    /** @return array{ok:false,backup:null,reason:string,reloaded:false,changed:false} */
    private function fail(string $reason): array
    {
        return ['ok' => false, 'backup' => null, 'reason' => $reason, 'reloaded' => false, 'changed' => false];
    }

    /** 机器上有没有 systemctl / service（决定「能不能替你重启」）。 */
    private function hasServiceManager(): bool
    {
        foreach (['systemctl', 'service'] as $binary) {
            [$code, $out] = $this->run('command -v ' . $binary . ' 2>/dev/null');
            if ($code === 0 && trim($out) !== '') {
                return true;
            }
        }

        return false;
    }

    /** 生成的 ini 块（带标记，便于重复执行时整块替换）。 */
    public function buildBlock(): string
    {
        $lines = [self::MARK_BEGIN];
        foreach ($this->mitigations() as $key => $value) {
            $lines[] = $key . ' = ' . $value;
        }
        $lines[] = self::MARK_END;

        return implode("\n", $lines) . "\n";
    }

    /** 去掉已有标记块（连同其上一行的空行），避免重复追加。 */
    private function stripBlock(string $content): string
    {
        $pattern = '/' . preg_quote(self::MARK_BEGIN, '/') . '.*?' . preg_quote(self::MARK_END, '/') . '\n?/s';

        return (string) preg_replace($pattern, '', $content);
    }

    /**
     * 用 php -c 验证 ini 可加载：跑一个临时探针脚本，输出必须是干净的 "ok"。
     *
     * 不用 `-r 'echo "ok";'`：引号要穿过 shell，Windows/MSYS 这类环境会把内层双引号吃掉
     * （实测 php 收到 `echo ok;` 后把 ok 当常量，直接 Fatal error —— 校验会误判成「ini 写坏了」）。
     * 探针文件还顺带覆盖了「php.ini 有错时 php 只打印 stderr 但 exit=0」的情况：
     * 输出里只要出现 error/warning/unknown 就按校验失败处理，宁可回滚。
     *
     * @return array{0:int,1:string}
     */
    private function validateIni(string $iniFile): array
    {
        $binary = defined('PHP_BINARY') ? (string) PHP_BINARY : '';
        if ($binary === '' || ! is_file($binary)) {
            return [0, '(跳过校验：找不到 php 二进制)'];
        }

        $probe = tempnam(sys_get_temp_dir(), 'phpini') ?: null;
        if ($probe === null) {
            return [0, '(跳过校验：无法创建探针文件)'];
        }

        $probeFile = $probe . '.php';
        @unlink($probe);
        @file_put_contents($probeFile, '<?php echo "ok";');

        try {
            [$code, $out] = $this->run(
                escapeshellarg($binary) . ' -c ' . escapeshellarg($iniFile) . ' ' . escapeshellarg($probeFile)
            );
        } finally {
            @unlink($probeFile);
        }

        if ($code !== 0) {
            return [$code, $out];
        }

        if (trim($out) !== 'ok' || preg_match('/error|warning|unknown|failed/i', $out)) {
            return [1, $out === '' ? '(探针无输出)' : $out];
        }

        return [0, $out];
    }

    private function restore(string $iniFile, string $original, string $backup): void
    {
        @file_put_contents($iniFile, $original);
        @unlink($backup);
    }

    /** @return array{0:int,1:string} */
    private function run(string $cmd): array
    {
        if ($this->runner !== null) {
            $result = ($this->runner)($cmd);

            return [(int) ($result[0] ?? 1), (string) ($result[1] ?? '')];
        }

        $output = [];
        $code = 0;
        @exec($cmd . ' 2>&1', $output, $code);

        return [$code, implode("\n", $output)];
    }

    private function firstLine(string $text): string
    {
        $line = trim((string) (strtok($text, "\n") ?: ''));

        return $line === '' ? '(无输出)' : $line;
    }
}
