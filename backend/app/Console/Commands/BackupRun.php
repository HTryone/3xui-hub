<?php

namespace App\Console\Commands;

use Illuminate\Console\Command;
use Symfony\Component\Process\Exception\ProcessFailedException;
use Symfony\Component\Process\Process;

/**
 * 数据库备份（每天 03:30 调度，见 routes/console.php；亦可 `3hub backup` 手动触发）。
 *
 * 设计要点：
 *   - mysqldump 的 stdout 通过 Process 分块回调直接写进 gzip，不落地大 .sql 中间文件；
 *   - 密码只在 MYSQL_PWD 环境变量里传，不进命令行（ps 看不到）；
 *   - dump 失败会删掉写了一半的 .gz —— 半截备份比没有备份更危险（看着像成功）；
 *   - 成功后按文件名（时间戳）倒序保留最新 keep 份，其余删除；目录不存在自动创建。
 */
class BackupRun extends Command
{
    protected $signature = 'backup:run';

    protected $description = '导出数据库备份到备份目录并按份数保留';

    public function handle(): int
    {
        $bin = (string) config('backup.mysqldump_bin', 'mysqldump');
        $dir = rtrim((string) config('backup.dir', '/var/backups/3xui-hub'), '/\\');
        $keep = max(1, (int) config('backup.keep', 7));

        if (! is_dir($dir) && ! @mkdir($dir, 0755, true) && ! is_dir($dir)) {
            $this->error("备份目录创建失败：{$dir}");

            return self::FAILURE;
        }

        $conn = (array) config('database.connections.mysql');
        $host = (string) ($conn['host'] ?? '127.0.0.1');
        $port = (string) ($conn['port'] ?? '3306');
        $database = (string) ($conn['database'] ?? '');
        $username = (string) ($conn['username'] ?? 'root');
        $password = (string) ($conn['password'] ?? '');

        if ($database === '') {
            $this->error('未配置数据库名（database.connections.mysql.database）');

            return self::FAILURE;
        }

        $target = $dir.'/backup-'.date('YmdHis').'.sql.gz';

        // 数组形式传参：不经过 shell，库名/主机名里的特殊字符不会被二次解释
        $args = [
            $bin,
            '--single-transaction',
            '--skip-lock-tables',
            '--host='.$host,
            '--port='.$port,
            '-u', $username,
            $database,
        ];

        // 密码非空才注入；空密码完全不带密码参数（与 mysql 客户端一致）
        $env = $password !== '' ? ['MYSQL_PWD' => $password] : null;

        $gz = @gzopen($target, 'wb');
        if ($gz === false) {
            $this->error("备份文件无法写入：{$target}");

            return self::FAILURE;
        }

        $stderr = '';
        $process = new Process($args, null, $env);
        $process->setTimeout(3600);
        $process->disableOutput();   // 只走回调流式写 gz，不在内存里再囤一份

        try {
            $process->run(function (string $type, string $buffer) use ($gz, &$stderr): void {
                if ($type === Process::OUT) {
                    gzwrite($gz, $buffer);
                } else {
                    $stderr .= $buffer;
                }
            });
        } catch (ProcessFailedException $e) {
            $stderr = $e->getMessage();
            $process->setExitCode(1);
        } catch (\Throwable $e) {
            // 启动失败（bin 不存在等）也走同一条「删半截文件」路径
            $stderr = $e->getMessage();
            $process->setExitCode(1);
        }

        gzclose($gz);

        if (! $process->isSuccessful()) {
            // 半截 .gz 必须删掉：否则下次 `ls` 看着像有备份，实际解不出
            @unlink($target);
            $this->error('备份失败（mysqldump 退出码 '.$process->getExitCode().'）');
            $summary = trim($stderr);
            if ($summary !== '') {
                $this->error(mb_substr($summary, 0, 2000));
            }

            return self::FAILURE;
        }

        $this->pruneOldBackups($dir, $keep);

        $size = (int) @filesize($target);
        $this->info('备份完成：'.$target);
        $this->line('大小：'.number_format($size / 1024, 1).' KB');

        return self::SUCCESS;
    }

    /**
     * 按文件名倒序只保留最新 $keep 份。文件名是 backup-YYYYmmddHHMMSS.sql.gz，
     * 定长时间戳 → 字典序即时间序，无需 stat 排序。
     */
    private function pruneOldBackups(string $dir, int $keep): void
    {
        $files = glob($dir.'/backup-*.sql.gz') ?: [];
        if (count($files) <= $keep) {
            return;
        }

        sort($files);   // 升序：最旧在前
        $expired = array_slice($files, 0, count($files) - $keep);

        foreach ($expired as $file) {
            @unlink($file);
        }
    }
}
