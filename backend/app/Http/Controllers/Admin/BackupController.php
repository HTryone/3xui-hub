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

        // 保留原文件到 tmp，import 时需要
        $tmpDir = $this->tmpDir();
        $savedPath = $tmpDir . '/backup_upload.' . $ext;
        copy($file->getRealPath(), $savedPath);

        try {
            $sqlPath = $this->extractBackupFile($savedPath, false);
        } catch (\Throwable $e) {
            return $this->error('无法读取备份文件：' . $e->getMessage(), 400);
        }

        try {
            $diff = $this->calculateDiffFromSql($sqlPath);
        } catch (\Throwable $e) {
            return $this->error('无法分析备份文件：' . $e->getMessage(), 400);
        }

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

    /**
     * 解析 mysqldump 文件中每个表的行数（通过 INSERT 语句计数）。
     */
    private function parseDumpTableCounts(string $sqlPath): array
    {
        $counts = [];
        $currentTable = null;

        $handle = fopen($sqlPath, 'r');
        if (!$handle) return $counts;

        while (($line = fgets($handle)) !== false) {
            // 匹配 INSERT INTO `table_name` 或 INSERT INTO `table_name` VALUES
            if (preg_match('/^INSERT\s+INTO\s+`?(\w+)`?\s/i', $line, $m)) {
                $currentTable = $m[1];
                if (!isset($counts[$currentTable])) {
                    $counts[$currentTable] = 0;
                }
            }

            // 在 INSERT 语句的延续行中累计值组（直到遇到分号结束）
            if ($currentTable !== null) {
                // 每个 "),(" 分隔一对值组，第一组没有前缀 "(" 所以 +1
                $counts[$currentTable] += substr_count($line, '),(') + 1;
                // 分号表示这条 INSERT 语句结束
                if (strpos($line, ';') !== false) {
                    $currentTable = null;
                }
            }
        }

        fclose($handle);
        return $counts;
    }

    /**
     * 导入数据库。
     * mode: overwrite（覆盖）| merge（增量合并）
     */
    public function import(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'mode' => ['required', 'in:overwrite,merge'],
            'restore_env' => ['nullable', 'boolean'],
            'site_url' => ['nullable', 'url'],
        ]);

        // 查找上传的备份文件
        $tmpDir = $this->tmpDir();
        $backupFile = null;
        foreach (['zip', 'sql'] as $ext) {
            $candidate = $tmpDir . '/backup_upload.' . $ext;
            if (File::exists($candidate)) {
                $backupFile = $candidate;
                break;
            }
        }

        if (!$backupFile) {
            return $this->error('请先上传备份文件', 400);
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

            // 如果用户指定了站点地址，更新 .env 中的 APP_URL
            if (!empty($data['site_url']) && !empty($data['restore_env'])) {
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

        $msg = $mode === 'overwrite' ? '已覆盖导入' : '已增量合并';
        if ($this->envNotice) {
            $msg .= '；' . $this->envNotice;
        }

        return $this->success(['env_notice' => $this->envNotice], $msg);
    }

    /**
     * 执行 SQL 文件导入（覆盖模式）。
     */
    private function overwriteImport(string $sqlPath): void
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
     */
    private function mergeImport(string $sqlPath): void
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
