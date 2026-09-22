<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        // 幂等：列已存在（备份导入 / 重复执行）→ 跳过，避免 1060 duplicate column
        if (Schema::hasColumn('plans', 'is_active')) {
            return;
        }

        Schema::table('plans', function (Blueprint $table) {
            $table->boolean('is_active')->default(true)->after('total_traffic')->comment('是否上架可购买');
        });
    }

    public function down(): void
    {
        Schema::table('plans', function (Blueprint $table) {
            $table->dropColumn('is_active');
        });
    }
};
