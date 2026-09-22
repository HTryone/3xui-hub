<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Services\BackupEnvService;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\File;
use Illuminate\Support\Facades\Process;
use Illuminate\Support\Facades\Artisan;

/**
 * 数据库备份管理（MySQL）。
 * GET /admin-api/backup/export    → 导出数据库
 * POST /admin-api/backup/import   → 导入数据库
 * POST /admin-api/backup/preview  → 预览差异
 *
 * .env 只搬 APP_KEY（跨机迁移解节点凭据用），其余行不动，详见 BackupEnvService。
 */
class BackupController extends Controller
{
    use ApiResponse;

    /** 上传文件的落盘名：preview() 写，import() 读。 */
    private const UPLOAD_BASE = 'backup_upload';

    /** 预览记录文件名（放在上传文件旁边）。 */
    private const PREVIEW_RECORD = 'backup_preview.json';

    /** 预览记录的有效期（分钟）：超过就得重新上传+重新预览。 */
    private const PREVIEW_TTL_MINUTES = 30;

    /** 本次导入 .env 恢复的提示信息（给前端显示）。 */
    private ?string $envNotice = null;

    public function __construct(private readonly BackupEnvService $envs)
    {
    }

    /**
     * 获取 MySQL 连接配置。
     */
    private function dbConfig(): array
    {
        return [
            'host' => config('database.connections.mysql.host', '127.0.0.1'),
            'port' => config('database.connections.mysql.port', '3306'),
            'database' => config('database.connections.mysql.database', 'controlhub'),
            'username' => config('database.connections.mysql.username', 'root'),
            'password' => config('database.connections.mysql.password', ''),
        ];
    }

    /**
     * 查找 mysqldump / mysql 命令路径。
     * 优先用 PATH 中的命令，找不到则从 DB_HOST 配置推断常见安装路径。
     */
    private function findMysqlTool(string $tool): string
    {
        // 1. PATH 中有直接可用的命令
        $checkCmd = PHP_OS_FAMILY === 'Windows' ? "where $tool 2>nul" : "which $tool 2>/dev/null";
        exec($checkCmd, $out, $code);
        if ($code === 0 && !empty($out)) {
            return $tool;
        }

        // 2. 常见安装路径（Windows 手动安装 / Linux BaoTa）
        $candidates = [];
        if (PHP_OS_FAMILY === 'Windows') {
            // 从 MySQL bin 目录推断
            foreach (['C:/Program Files/MySQL', 'F:/wykf/mysql'] as $base) {
                if (is_dir($base)) {
                    $dirs = glob($base . '/mysql-*/bin/' . $tool . '.exe');
                    if ($dirs) { $candidates[] = $dirs[0]; }
                }
            }
        } else {
            // Linux BaoTa / 常见路径
            $candidates = [
                "/usr/bin/$tool",
                "/usr/local/bin/$tool",
                "/www/server/mysql/bin/$tool",
            ];
        }

        foreach ($candidates as $path) {
            if (file_exists($path)) {
                return $path;
            }
        }

        // 3. 兜底返回命令名（让调用方报错）
        return $tool;
    }

    /**
     * 确保 tmp 目录存在。
     */
    private function tmpDir(): string
    {
        $dir = storage_path('app/tmp');
        if (!File::isDirectory($dir)) {
            File::makeDirectory($dir, 0755, true);
        }
        return $dir;
    }

    /**
     * 获取 stderr 重定向到空设备的命令片段。
     * Windows: 2>nul  |  Linux: 2>/dev/null
     */
    private function stderrNull(): string
    {
        return PHP_OS_FAMILY === 'Windows' ? '2>nul' : '2>/dev/null';
    }

    /** 上传备份文件的落盘路径。 */
    private function uploadPath(string $tmpDir, string $ext): string
    {
        return $tmpDir . '/' . self::UPLOAD_BASE . '.' . $ext;
    }

    /** 预览记录的路径。 */
    private function previewRecordPath(string $tmpDir): string
    {
        return $tmpDir . '/' . self::PREVIEW_RECORD;
    }

    /**
     * 找到当前暂存的上传备份（preview() 落盘的那份）。
     */
    private function findUploadedBackup(string $tmpDir): ?string
    {
        foreach (['zip', 'sql'] as $ext) {
            $candidate = $this->uploadPath($tmpDir, $ext);
            if (File::exists($candidate)) {
                return $candidate;
            }
        }

        return null;
    }

    /**
     * 清掉磁盘上一切与上传/预览有关的中间文件。
     *
     * 里面装的是用户整库数据，不能让它们长期躺在 storage/app/tmp 里：预览完的
     * 那份解析用 SQL（backup_preview.sql）、放弃掉的上传、以及预览记录本身。
     */
    private function clearPreviewArtifacts(string $tmpDir): void
    {
        foreach (['zip', 'sql'] as $ext) {
            File::delete($this->uploadPath($tmpDir, $ext));
        }
        File::delete($tmpDir . '/backup_preview.sql');
        File::delete($this->previewRecordPath($tmpDir));
    }

    /**
     * 记下「这份上传刚刚被预览过」：文件名 + 内容指纹 + 时间戳。
     *
     * 放磁盘文件而不是 cache / session —— 本项目 CACHE_STORE 与 SESSION_DRIVER 都是
     * database，而覆盖导入会把整个库（连同 cache / sessions 表）替换成备份里的老版本：
     * 恰恰在最需要这层保护的场景下，存在库里的记录会先被冲掉（导入流程里的
     * cache:clear 同样会带走它）。storage/app/tmp 下的文件不受影响。
     */
    private function writePreviewRecord(string $tmpDir, string $uploadPath): void
    {
        File::put($this->previewRecordPath($tmpDir), json_encode([
            'file' => basename($uploadPath),
            'sha256' => (string) hash_file('sha256', $uploadPath),
            'size' => File::size($uploadPath),
            'previewed_at' => now()->getTimestamp(),
            'expires_at' => now()->addMinutes(self::PREVIEW_TTL_MINUTES)->getTimestamp(),
        ]));
    }

    /**
     * 校验「这次 import 用的就是刚预览过的那份上传」，校验完记录即作废（一次性）。
     *
     * 挡的就是：预览完没导入就关了弹窗，过一阵（甚至换了个人）再来点导入 ——
     * 那时盘上还躺着上一份 backup_upload.*，照旧会被当成本次备份灌进库。
     *
     * @return string|null 拒绝原因；null 表示校验通过
     */
    private function consumePreviewRecord(string $tmpDir, string $uploadPath): ?string
    {
        $recordPath = $this->previewRecordPath($tmpDir);
        $retry = '，请重新上传备份文件、预览差异后再导入';

        if (!File::exists($recordPath)) {
            return '没有找到本次上传的预览记录' . $retry;
        }

        $record = json_decode((string) File::get($recordPath), true);
        // 一次性：不论校验过不过，这份记录都不再复用（要导就重新走一遍预览）
        File::delete($recordPath);

        if (!is_array($record)) {
            return '预览记录已损坏' . $retry;
        }

        if ((int) ($record['expires_at'] ?? 0) < now()->getTimestamp()) {
            return '这份上传的预览已超过 ' . self::PREVIEW_TTL_MINUTES . ' 分钟' . $retry;
        }

        if (($record['file'] ?? '') !== basename($uploadPath)
            || ($record['sha256'] ?? '') !== (string) hash_file('sha256', $uploadPath)) {
            return '上传的备份文件与预览过的不是同一份' . $retry;
        }

        return null;
    }

    /**
     * 导入收尾①：补跑新代码需要、但老面板的 dump 里没有的迁移。
     *
     * dump 自带老面板的建表语句**和 migrations 记录**，所以这里只会补缺的那几个
     * （本仓迁移都是幂等、只增的，重复执行安全）。没有这一步，导完 domains 表、
     * users.traffic_disabled_at 之类的结构就是缺的，只能人工敲 migrate。
     *
     * @return int 本次实际执行的迁移数
     * @throws \RuntimeException migrate 非 0 退出
     */
    private function runPendingMigrations(): int
    {
        $exitCode = Artisan::call('migrate', ['--force' => true]);
        $output = (string) Artisan::output();

        if ($exitCode !== 0) {
            throw new \RuntimeException('migrate 退出码 ' . $exitCode . ($output === '' ? '' : '：' . trim($output)));
        }

        // Laravel 每个迁移打一行 "Migrating: xxx"，拿它数本次跑了几个
        return substr_count($output, 'Migrating:');
    }

    /**
     * 导入收尾②：给常驻队列 worker 发重启信号。
     *
     * 导入会换 APP_KEY，而 worker 是常驻进程，不重启就还拿着旧密钥（节点凭据解不开）。
     * 只发信号不碰 systemctl：队列由 systemd 管着，收到信号优雅退出后会被重新拉起，
     * 从而读到新的 .env —— 也就不需要 sudo（现网 sudoers 只放行了 3hub-domain）。
     *
     * @throws \RuntimeException queue:restart 非 0 退出
     */
    private function restartQueueWorkers(): void
    {
        $exitCode = Artisan::call('queue:restart');
        if ($exitCode !== 0) {
            throw new \RuntimeException('queue:restart 退出码 ' . $exitCode);
        }
    }

    /**
     * 导出数据库（zip包：dump.sql + 只含 APP_KEY 的 .env）。
     */
    public function export()
    {
        $cfg = $this->dbConfig();
        $filename = 'controlhub-backup-' . date('YmdHis') . '.zip';
        $tmpDir = $this->tmpDir();
        $dumpPath = $tmpDir . '/dump.sql';

        // mysqldump 导出（直接重定向到文件，避免 exec 捕获时编码损坏）
        $mysqldump = $this->findMysqlTool('mysqldump');
        // --set-gtid-purged=OFF 是 MySQL 专用，MariaDB 不支持
        $isMariaDb = stripos(shell_exec('mysql --version ' . $this->stderrNull()) ?? '', 'mariadb') !== false;
        $gtidOpt = $isMariaDb ? '' : ' --set-gtid-purged=OFF';
        $cmd = sprintf(
            '%s --host=%s --port=%s --user=%s --password=%s --single-transaction --routines --triggers%s %s > %s ' . $this->stderrNull(),
            escapeshellarg($mysqldump),
            escapeshellarg($cfg['host']),
            escapeshellarg($cfg['port']),
            escapeshellarg($cfg['username']),
            escapeshellarg($cfg['password']),
            $gtidOpt,
            escapeshellarg($cfg['database']),
            escapeshellarg($dumpPath)
        );
        $exitCode = 0;
        exec($cmd, $output, $exitCode);

        if ($exitCode !== 0 || !File::exists($dumpPath) || File::size($dumpPath) === 0) {
            File::delete($dumpPath);
            return $this->error('数据库导出失败（mysqldump 返回 ' . $exitCode . '）', 500);
        }

        // 打包 zip
        $tmpPath = $tmpDir . '/' . $filename;
        try {
            $this->buildBackupZip($dumpPath, $tmpPath);
        } catch (\RuntimeException $e) {
            File::delete($dumpPath);
            return $this->error($e->getMessage(), 500);
        }
        File::delete($dumpPath);

        $content = file_get_contents($tmpPath);
        File::delete($tmpPath);

        return response($content, 200, [
            'Content-Type' => 'application/zip',
            'Content-Disposition' => 'attachment; filename="' . $filename . '"',
            'Content-Length' => strlen($content),
        ]);
    }

    /**
     * 打包备份 zip：dump.sql + 只含 APP_KEY 的 .env。
     *
     * 不再把整份 .env 塞进备份：DB 密码 / QUEUE_CONNECTION / 邮件配置都是本机自己的，
     * 带出去既泄密，也让"导入整份覆盖 .env"这类误操作有破坏力（曾把队列冲成 sync）。
     *
     * @throws \RuntimeException 无法创建 zip 时
     */
    public function buildBackupZip(string $dumpPath, string $zipPath): void
    {
        $zip = new \ZipArchive();
        if ($zip->open($zipPath, \ZipArchive::CREATE | \ZipArchive::OVERWRITE) !== true) {
            throw new \RuntimeException('无法创建备份文件');
        }

        $zip->addFile($dumpPath, 'dump.sql');

        // 条目名仍是 .env，老的解析逻辑 / 前端 has_env 判断不受影响
        $envContents = $this->envs->appKeyOnlyContents();
        if ($envContents !== null) {
            $zip->addFromString('.env', $envContents);
        }

        $zip->close();
    }

    /**
     * 解压备份文件，返回 SQL 文件路径。兼容 .zip 和 .sql。
     * $restoreEnv: 是否从备份恢复 APP_KEY（仅导入时恢复）。
     */
    private function extractBackupFile(string $filePath, bool $restoreEnv = false): string
    {
        $tmpDir = $this->tmpDir();
        $ext = strtolower(pathinfo($filePath, PATHINFO_EXTENSION));

        if ($ext === 'zip') {
            $zip = new \ZipArchive();
            if ($zip->open($filePath) !== true) {
                throw new \RuntimeException('无法打开 ZIP 文件');
            }

            // 导入时才恢复 .env：只把备份里的 APP_KEY 搬过来，本机其余行一个字都不动
            if ($restoreEnv) {
                $envIndex = $zip->locateName('.env');
                $this->envNotice = $this->envs->restoreAppKeyFromEnvContents(
                    $envIndex === false ? null : (string) $zip->getFromIndex($envIndex)
                );
            }

            // 提取 dump.sql
            $sqlIndex = $zip->locateName('dump.sql');
            if ($sqlIndex === false) {
                $zip->close();
                // 兼容旧格式
                $oldIndex = $zip->locateName('database.sqlite');
                if ($oldIndex !== false) {
                    throw new \RuntimeException('这是旧版 SQLite 备份文件，无法在 MySQL 模式下导入');
                }
                throw new \RuntimeException('ZIP 中未找到 dump.sql');
            }

            $sqlTmpPath = $tmpDir . '/backup_preview.sql';
            file_put_contents($sqlTmpPath, $zip->getFromIndex($sqlIndex));
            $zip->close();

            return $sqlTmpPath;
        } elseif ($ext === 'sql') {
            // 直接 .sql 文件
            $sqlTmpPath = $tmpDir . '/backup_preview.sql';
            copy($filePath, $sqlTmpPath);
            return $sqlTmpPath;
        } else {
            throw new \RuntimeException('不支持的备份文件格式：' . $ext);
        }
    }

    /**
     * 预览导入差异。
     */
    public function preview(Request $request): \Illuminate\Http\JsonResponse
    {
        $request->validate([
            'file' => ['required', 'file', 'max:51200'], // 50MB
        ]);

        $file = $request->file('file');
        $ext = strtolower(pathinfo($file->getClientOriginalName(), PATHINFO_EXTENSION));

        $tmpDir = $this->tmpDir();

        // 先把上一轮留下的东西全清掉：上一次放弃的上传、上一条预览记录、
        // 以及解析用的中间 SQL。盘上任何时刻只允许存在「本次这一份」。
        $this->clearPreviewArtifacts($tmpDir);

        // 保留原文件到 tmp，import 时需要
        $savedPath = $this->uploadPath($tmpDir, $ext);
        copy($file->getRealPath(), $savedPath);

        try {
            $sqlPath = $this->extractBackupFile($savedPath, false);
        } catch (\Throwable $e) {
            $this->clearPreviewArtifacts($tmpDir);
            return $this->error('无法读取备份文件：' . $e->getMessage(), 400);
        }

        try {
            $diff = $this->calculateDiffFromSql($sqlPath);
        } catch (\Throwable $e) {
            File::delete($sqlPath);
            $this->clearPreviewArtifacts($tmpDir);
            return $this->error('无法分析备份文件：' . $e->getMessage(), 400);
        }

        // 解析用的 SQL 是用户整库数据，预览完立刻删；import() 会自己再解一遍
        File::delete($sqlPath);

        // 检查备份是否包含 .env
        $hasEnv = false;
        if ($ext === 'zip') {
            $zip = new \ZipArchive();
            if ($zip->open($savedPath) === true) {
                $hasEnv = $zip->locateName('.env') !== false;
                $zip->close();
            }
        }

        $backupEnv = $hasEnv ? $this->envs->readBackupEnv($savedPath) : null;
        $currentEnv = $this->envs->parseEnv($this->envs->envPath());

        // 记下「这份上传刚预览过」，import() 只认带这条记录的上传
        $this->writePreviewRecord($tmpDir, $savedPath);

        return $this->success([
            'diff' => $diff,
            'has_env' => $hasEnv,
            'env_diff' => $backupEnv ? $this->envs->envDiff($backupEnv) : [],
            // 站点地址默认填本机当前值；备份里的旧域名只作提示
            'current_app_url' => $currentEnv['APP_URL'] ?? '',
            'backup_app_url' => $backupEnv['APP_URL'] ?? '',
        ]);
    }

    /**
     * 从 SQL dump 文件解析各表行数差异。
     */
    private function calculateDiffFromSql(string $sqlPath): array
    {
        $tables = ['users', 'nodes', 'plans', 'orders', 'payment_configs', 'admins', 'node_inbounds'];

        // 解析 dump.sql 中每个表的 INSERT 行数
        $importCounts = $this->parseDumpTableCounts($sqlPath);

        $diff = [];
        foreach ($tables as $table) {
            $currentCount = DB::table($table)->count();
            $importCount = $importCounts[$table] ?? 0;

            $diff[] = [
                'table' => $table,
                'current' => $currentCount,
                'import' => $importCount,
                'new' => max(0, $importCount - $currentCount),
            ];
        }

        return $diff;
    }

    /** 解析状态：语句外，等 INSERT 关键字。 */
    private const SQL_IDLE = 0;
    /** 解析状态：已见 INSERT，等 INTO（可夹 INSERT IGNORE INTO）。 */
    private const SQL_INTO = 1;
    /** 解析状态：已见 INTO，正在读表名。 */
    private const SQL_TABLE = 2;
    /** 解析状态：已取到表名，等 VALUES（中间可能还有列名列表，不计数）。 */
    private const SQL_HEAD = 3;
    /** 解析状态：已进入值组区，按括号深度计数。 */
    private const SQL_VALUES = 4;

    /**
     * 解析 mysqldump 文件中每个表的行数（通过 INSERT 语句计数）。
     *
     * 按括号深度数「值组」：进入 VALUES 之后，每遇到一次深度 0 → 1 的 "(" 就是一个
     * 值组。列名列表在 VALUES 之前，天然不参与计数。
     *
     * 早先的写法是每行 `substr_count('),(') + 1`，遇上一表一条 INSERT、每行一个值组的
     * mysqldump 输出（行尾是 `),` 不是 `),(`）会按行数多算：头行 +1、每个数据行 +1，
     * 于是每张表恒多 1。字符串里的 "),(" 也会被误判。
     */
    private function parseDumpTableCounts(string $sqlPath): array
    {
        $counts = [];
        $state = [
            'phase' => self::SQL_IDLE,
            'table' => null,      // 当前 INSERT 的目标表；null = 语句外
            'name' => '',         // SQL_TABLE 阶段攒起来的表名
            'quoted' => false,    // 是否处在一对反引号之间
            'depth' => 0,         // 值组括号深度
            'string' => false,    // 是否处在单引号字符串里
            'escape' => false,    // 上一个字符是反斜杠（字符串内转义）
            'word' => '',         // 攒到一半的关键字 / 标识符
        ];

        $handle = fopen($sqlPath, 'r');
        if (!$handle) return $counts;

        while (($line = fgets($handle)) !== false) {
            $len = strlen($line);
            for ($i = 0; $i < $len; $i++) {
                $this->feedSqlChar($line[$i], $state, $counts);
            }
        }

        fclose($handle);
        return $counts;
    }

    /**
     * 把一个字符喂给解析状态机（状态跨行保持，所以跨行 INSERT 也能数对）。
     */
    private function feedSqlChar(string $ch, array &$state, array &$counts): void
    {
        // 字符串内容不是 SQL 结构：里面的 "(" ")" ";" 以及 \' \\ 转义都不参与计数
        if ($state['string']) {
            if ($state['escape']) {
                $state['escape'] = false;
            } elseif ($ch === '\\') {
                $state['escape'] = true;
            } elseif ($ch === "'") {
                $state['string'] = false;
            }
            return;
        }

        // 关键字 / 标识符按「攒到非单词字符为止」收尾，避免 VALUES 紧贴 "(" 时漏字符
        if (self::isSqlWordChar($ch)) {
            if ($state['phase'] === self::SQL_TABLE) {
                $state['name'] .= $ch;
            }
            $state['word'] .= $ch;
            return;
        }

        $word = strtolower($state['word']);
        $state['word'] = '';

        switch ($state['phase']) {
            case self::SQL_IDLE:
                if ($word === 'insert') {
                    $state['phase'] = self::SQL_INTO;
                }
                break;

            case self::SQL_INTO:
                if ($word === 'ignore') break; // INSERT IGNORE INTO
                $state['phase'] = $word === 'into' ? self::SQL_TABLE : self::SQL_IDLE;
                if ($state['phase'] === self::SQL_TABLE) {
                    $state['name'] = '';
                }
                break;

            case self::SQL_TABLE:
                if ($ch === '`') {
                    $state['quoted'] = !$state['quoted'];
                    break;
                }
                if ($state['name'] !== '') {
                    $state['table'] = $state['name'];
                    $state['name'] = '';
                    $counts[$state['table']] = $counts[$state['table']] ?? 0;
                    $state['phase'] = self::SQL_HEAD;
                }
                break;

            case self::SQL_HEAD:
                if ($ch === '`') {
                    $state['quoted'] = !$state['quoted'];
                    break;
                }
                if ($word === 'values' && !$state['quoted']) {
                    $state['phase'] = self::SQL_VALUES;
                    $state['depth'] = 0;
                    // VALUES 与第一个值组可能紧贴着写：VALUES(1),(2)
                    $this->countValueGroupChar($ch, $state, $counts);
                }
                break;

            case self::SQL_VALUES:
                $this->countValueGroupChar($ch, $state, $counts);
                break;
        }
    }

    /**
     * 值组区里的一次字符推进：数深度 0 → 1 的括号，分号收尾。
     */
    private function countValueGroupChar(string $ch, array &$state, array &$counts): void
    {
        if ($ch === "'") {
            $state['string'] = true;
        } elseif ($ch === '(') {
            if ($state['depth'] === 0 && $state['table'] !== null) {
                $counts[$state['table']]++;
            }
            $state['depth']++;
        } elseif ($ch === ')') {
            if ($state['depth'] > 0) {
                $state['depth']--;
            }
        } elseif ($ch === ';' && $state['depth'] === 0) {
            // 语句结束，回到找 INSERT 的状态（同一行后面可能还有别的语句）
            $state['phase'] = self::SQL_IDLE;
            $state['table'] = null;
            $state['name'] = '';
            $state['quoted'] = false;
        }
    }

    /** SQL 里算作关键字 / 标识符的字符（>= 0x80 的字节留给多字节表名）。 */
    private static function isSqlWordChar(string $ch): bool
    {
        return ctype_alnum($ch) || $ch === '_' || $ch === '$' || ord($ch) >= 0x80;
    }

    /**
     * 导入数据库。
     * mode: overwrite（覆盖）| merge（增量合并）
     *
     * 数据进库之后还有两件收尾，都是为了消掉人工步骤（详见 runPendingMigrations /
     * restartQueueWorkers 的注释）：补跑迁移、让队列 worker 重启吃新 APP_KEY。
     * 这两步失败**不回滚已导入的数据**（回滚比缺个迁移更危险），但也不许静默。
     */
    public function import(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'mode' => ['required', 'in:overwrite,merge'],
            'restore_env' => ['nullable', 'boolean'],
            'site_url' => ['nullable', 'url'],
        ]);

        $tmpDir = $this->tmpDir();
        $backupFile = $this->findUploadedBackup($tmpDir);

        if (!$backupFile) {
            return $this->error('请先上传备份文件', 400);
        }

        // 只认「刚预览过的那一份上传」，剩下的文件 + 记录一起清掉：
        // 预览完放弃、过一阵再来点导入，不能把上一份陈旧备份灌进库。
        $stale = $this->consumePreviewRecord($tmpDir, $backupFile);
        if ($stale !== null) {
            $this->clearPreviewArtifacts($tmpDir);
            return $this->error($stale, 400);
        }

        $mode = $data['mode'];
        $this->envNotice = null;

        try {
            // 导入时从备份恢复 APP_KEY（如果是 zip）：只改这一行，其余行不动
            $sqlPath = $this->extractBackupFile($backupFile, !empty($data['restore_env']));

            // 改过 .env 后清配置缓存（确保 APP_KEY 等立即生效）
            if (!empty($data['restore_env'])) {
                \Artisan::call('config:clear');
                \Artisan::call('cache:clear');
            }

            // 用户指定了站点地址才改本机 APP_URL（留空 = 一个字都不动，备份里的旧域名
            // 永远不会自动写进来）。只看 site_url，不看 restore_env：改域名与「要不要
            // 恢复 APP_KEY」是两件事——取消勾选 APP_KEY 时用户照样可能要改成新机器的域名。
            if (!empty($data['site_url'])) {
                $this->envs->updateKey('APP_URL', $data['site_url']);
            }

            if ($mode === 'overwrite') {
                $this->overwriteImport($sqlPath);
            } else {
                $this->mergeImport($sqlPath);
            }
        } catch (\Throwable $e) {
            return $this->error('导入失败：' . $e->getMessage(), 500);
        } finally {
            if (isset($sqlPath)) File::delete($sqlPath);
            File::delete($backupFile);
        }

        // ===== 数据处理完了，下面是收尾 =====
        // 两步各自独立 try/catch：迁移挂了也要照发队列重启信号，反之亦然。
        $notices = [];
        $failures = [];
        $migrateRan = null;
        $queueRestarted = false;

        try {
            $migrateRan = $this->runPendingMigrations();
            $notices[] = $migrateRan > 0 ? "数据库迁移：补跑了 {$migrateRan} 个" : '数据库迁移：无新迁移';
        } catch (\Throwable $e) {
            $failures[] = '数据库迁移失败：' . $e->getMessage() . '（可手动执行 php artisan migrate --force）';
        }

        try {
            $this->restartQueueWorkers();
            $queueRestarted = true;
            $notices[] = '队列 worker 已重启（会读到新的 APP_KEY）';
        } catch (\Throwable $e) {
            $failures[] = '队列重启失败：' . $e->getMessage() . '（可手动执行 php artisan queue:restart）';
        }

        $msg = $mode === 'overwrite' ? '已覆盖导入' : '已增量合并';
        if ($this->envNotice) {
            $msg .= '；' . $this->envNotice;
        }
        $msg .= '；' . implode('；', $notices);

        $payload = [
            'env_notice' => $this->envNotice,
            'notice' => $msg,
            'migrate_ran' => $migrateRan,
            'queue_restarted' => $queueRestarted,
        ];

        if ($failures) {
            // 数据已经在库里了，这里只报「收尾没做完」+ 怎么办，不假装导入失败
            return $this->error('数据已导入，但' . implode('；', $failures) . '。已导入的数据未回滚。', 500);
        }

        return $this->success($payload, $msg);
    }

    /**
     * 执行 SQL 文件导入（覆盖模式）。
     *
     * protected：测试里用子类替掉真正的 mysql 导入，只验收尾流程。
     */
    protected function overwriteImport(string $sqlPath): void
    {
        $cfg = $this->dbConfig();
        $mysql = $this->findMysqlTool('mysql');

        $cmd = sprintf(
            '%s --host=%s --port=%s --user=%s --password=%s %s < %s ' . $this->stderrNull(),
            escapeshellarg($mysql),
            escapeshellarg($cfg['host']),
            escapeshellarg($cfg['port']),
            escapeshellarg($cfg['username']),
            escapeshellarg($cfg['password']),
            escapeshellarg($cfg['database']),
            escapeshellarg($sqlPath)
        );

        $output = [];
        $exitCode = 0;
        exec($cmd, $output, $exitCode);

        if ($exitCode !== 0) {
            throw new \RuntimeException('SQL 导入失败：' . implode("\n", $output));
        }
    }

    /**
     * 增量合并导入。
     * 先导入到临时库，再逐表合并到主库。
     *
     * protected：同 overwriteImport()，留给测试替换。
     */
    protected function mergeImport(string $sqlPath): void
    {
        $cfg = $this->dbConfig();
        $mysql = $this->findMysqlTool('mysql');
        $tmpDb = 'controlhub_merge_tmp_' . time();

        // 创建临时数据库
        DB::statement("CREATE DATABASE `{$tmpDb}` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci");

        try {
            // 导入 SQL 到临时库
            $cmd = sprintf(
                '%s --host=%s --port=%s --user=%s --password=%s %s < %s ' . $this->stderrNull(),
                escapeshellarg($mysql),
                escapeshellarg($cfg['host']),
                escapeshellarg($cfg['port']),
                escapeshellarg($cfg['username']),
                escapeshellarg($cfg['password']),
                escapeshellarg($tmpDb),
                escapeshellarg($sqlPath)
            );

            $output = [];
            $exitCode = 0;
            exec($cmd, $output, $exitCode);

            if ($exitCode !== 0) {
                throw new \RuntimeException('临时库导入失败：' . implode("\n", $output));
            }

            // 配置临时库连接
            config()->set("database.connections.merge_tmp", [
                'driver' => 'mysql',
                'host' => $cfg['host'],
                'port' => $cfg['port'],
                'database' => $tmpDb,
                'username' => $cfg['username'],
                'password' => $cfg['password'],
                'charset' => 'utf8mb4',
                'collation' => 'utf8mb4_unicode_ci',
            ]);

            $importDb = DB::connection('merge_tmp');

            DB::transaction(function () use ($importDb) {
                // 合并套餐
                foreach ($importDb->table('plans')->get() as $plan) {
                    if (!DB::table('plans')->where('name', $plan->name)->exists()) {
                        DB::table('plans')->insert((array) $plan);
                    }
                }

                // 合并用户
                foreach ($importDb->table('users')->get() as $user) {
                    if ($user->email && !DB::table('users')->where('email', $user->email)->exists()) {
                        DB::table('users')->insert((array) $user);
                    }
                }

                // 合并节点
                foreach ($importDb->table('nodes')->get() as $node) {
                    if (!DB::table('nodes')->where('host', $node->host)->where('port', $node->port)->exists()) {
                        DB::table('nodes')->insert((array) $node);
                    }
                }

                // 合并支付配置
                foreach ($importDb->table('payment_configs')->get() as $payment) {
                    if (!DB::table('payment_configs')->where('name', $payment->name)->exists()) {
                        DB::table('payment_configs')->insert((array) $payment);
                    }
                }

                // 合并节点入站
                foreach ($importDb->table('node_inbounds')->get() as $inbound) {
                    if (!DB::table('node_inbounds')
                        ->where('node_id', $inbound->node_id)
                        ->where('protocol', $inbound->protocol)
                        ->where('inbound_id', $inbound->inbound_id)
                        ->exists()) {
                        DB::table('node_inbounds')->insert((array) $inbound);
                    }
                }

                // 合并订单
                foreach ($importDb->table('orders')->get() as $order) {
                    if (!DB::table('orders')->where('order_no', $order->order_no)->exists()) {
                        DB::table('orders')->insert((array) $order);
                    }
                }
            });
        } finally {
            // 删除临时数据库
            try {
                DB::statement("DROP DATABASE IF EXISTS `{$tmpDb}`");
            } catch (\Throwable $e) {
                \Log::warning("Failed to drop temp DB {$tmpDb}: " . $e->getMessage());
            }
        }
    }
}
