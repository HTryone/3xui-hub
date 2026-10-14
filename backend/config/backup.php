<?php

return [

    /*
    |--------------------------------------------------------------------------
    | 数据库备份
    |--------------------------------------------------------------------------
    |
    | `backup:run` 命令（每天 03:30 调度，见 routes/console.php）的落盘位置、
    | 保留份数与 mysqldump 可执行文件。三项都可用 env 覆盖：本机测试时把
    | BACKUP_MYSQLDUMP_BIN 指向真实的 mysqldump（发行版通常已在 PATH 里）。
    |
    */

    // 备份目录：不存在时命令会自动创建（部署侧另有一道 mkdir + chown 自愈）
    'dir' => env('BACKUP_DIR', '/var/backups/3xui-hub'),

    // 按文件名（时间戳）倒序只保留最新 N 份，更旧的自动删除
    'keep' => (int) env('BACKUP_KEEP', 7),

    // mysqldump 可执行文件：默认走 PATH；本机（Windows）测试时注入绝对路径
    'mysqldump_bin' => env('BACKUP_MYSQLDUMP_BIN', 'mysqldump'),

];
