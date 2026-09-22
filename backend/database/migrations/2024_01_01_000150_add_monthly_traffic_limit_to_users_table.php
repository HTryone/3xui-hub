<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 用户表添加 monthly_traffic_limit：记录购买时的套餐月流量限额。
 * 套餐修改后不影响已购买的用户。
 */
return new class extends Migration
{
    public function up(): void
    {
        // 幂等：列已存在（备份导入 / 重复执行）→ 跳过，避免 1060 duplicate column
        if (Schema::hasColumn('users', 'monthly_traffic_limit')) {
            return;
        }

        Schema::table('users', function (Blueprint $table) {
            $table->bigInteger('monthly_traffic_limit')->default(0)->after('monthly_traffic_used');
        });
    }

    public function down(): void
    {
        Schema::table('users', function (Blueprint $table) {
            $table->dropColumn('monthly_traffic_limit');
        });
    }
};
