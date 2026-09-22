<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * users.traffic_disabled_at：最近一次【确认关闭】3x-ui 流量的时间。
 *
 * 用途：扫描器（BanCheckJob / SyncTrafficCommand）据此跳过「已经关掉且未超时效」的用户，
 * 避免同一批超限用户每 5 分钟被重复关闭（每用户每轮 19 次节点请求）。
 * null = 未关闭（或已重新开启）。超时效后仍会重新校验一次，兜住面板侧的状态漂移。
 */
return new class extends Migration
{
    public function up(): void
    {
        // 幂等：列已存在（重复执行 / 手工加过列）时不再重复添加
        if (Schema::hasColumn('users', 'traffic_disabled_at')) {
            return;
        }

        Schema::table('users', function (Blueprint $table) {
            $table->timestamp('traffic_disabled_at')->nullable()->after('enabled')
                ->comment('最近一次确认关闭 3x-ui 流量的时间，null=未关闭');
        });
    }

    public function down(): void
    {
        if (!Schema::hasColumn('users', 'traffic_disabled_at')) {
            return;
        }

        Schema::table('users', function (Blueprint $table) {
            $table->dropColumn('traffic_disabled_at');
        });
    }
};
