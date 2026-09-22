<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Support\Facades\Schema;
use Illuminate\Database\Schema\Blueprint;

return new class extends Migration
{
    public function up(): void
    {
        // 幂等：列已存在（备份导入 / 重复执行）→ 跳过，避免 1060 duplicate column
        if (Schema::hasColumn('plans', 'reset_price')) {
            return;
        }

        Schema::table('plans', function (Blueprint $table) {
            $table->decimal('reset_price', 10, 2)->default(0)->after('price')->comment('重置流量价格(元)');
        });
    }

    public function down(): void
    {
        Schema::table('plans', function (Blueprint $table) {
            $table->dropColumn('reset_price');
        });
    }
};
