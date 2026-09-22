<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        // 幂等：两列逐列判断（备份导入 / 重复执行）→ 已存在的列跳过，避免 1060。
        // 逐列而非整段跳过：dump 带来的老表可能只加了其中一列。
        Schema::table('users', function (Blueprint $table) {
            if (! Schema::hasColumn('users', 'plan_id')) {
                $table->foreignId('plan_id')->nullable()->after('protocol')->constrained('plans')->nullOnDelete();
            }

            if (! Schema::hasColumn('users', 'monthly_traffic_used')) {
                $table->bigInteger('monthly_traffic_used')->default(0)->after('traffic_used');
            }
        });
    }

    public function down(): void
    {
        Schema::table('users', function (Blueprint $table) {
            $table->dropForeign(['plan_id']);
            $table->dropColumn(['plan_id', 'monthly_traffic_used']);
        });
    }
};
