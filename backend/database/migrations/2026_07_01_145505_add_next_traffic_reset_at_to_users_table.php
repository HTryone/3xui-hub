<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        // 幂等：列已存在（备份导入 / 重复执行）→ 跳过，避免 1060 duplicate column
        if (Schema::hasColumn('users', 'next_traffic_reset_at')) {
            return;
        }

        Schema::table('users', function (Blueprint $table) {
            $table->timestamp('next_traffic_reset_at')->nullable()->after('monthly_traffic_limit')->comment('下次流量重置时间');
        });
    }

    public function down(): void
    {
        Schema::table('users', function (Blueprint $table) {
            $table->dropColumn('next_traffic_reset_at');
        });
    }
};
