<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 节点运行指标：把 3x-ui 面板节点的 CPU / 内存占用存下来（仪表盘监控用）。
 *
 * 背景：HealthCheckJob 每 5 分钟请求一次 /panel/api/server/status，响应里本来就有
 * cpu / mem，但 HealthCheckService::apply() 只写 status/latency，这两个值当场丢弃。
 * 仪表盘要展示节点监控，就得把它们留下来 —— 不增加任何对节点的请求。
 *
 * 两列只服务 3x-ui 面板节点。xray 裸内核节点不用它们：cpu/mem 由 agent 每 5 秒上报，
 * 存在 driver_config.agent 里（NodeWsServer / NodeApiController 维护）。
 *
 * 可空：老节点、尚未跑过健康检查的节点为 NULL，前端按「无数据」处理。
 */
return new class extends Migration
{
    public function up(): void
    {
        // 逐列守卫（照 2026_09_30_000001 的写法）：备份 dump 带来的老 nodes 表可能已经
        // 有这列但没有迁移记录，不守卫就是 1060 重复列。
        Schema::table('nodes', function (Blueprint $table) {
            if (! Schema::hasColumn('nodes', 'cpu')) {
                $table->decimal('cpu', 5, 2)->nullable()->default(null)->after('latency');
            }
            if (! Schema::hasColumn('nodes', 'mem')) {
                $table->decimal('mem', 5, 2)->nullable()->default(null)->after('cpu');
            }
        });
    }

    public function down(): void
    {
        Schema::table('nodes', function (Blueprint $table) {
            $table->dropColumn(['cpu', 'mem']);
        });
    }
};
