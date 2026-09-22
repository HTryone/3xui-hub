<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        // 幂等：列已存在（备份导入 / 重复执行）→ 跳过，避免 1060 duplicate column
        if (Schema::hasColumn('plans', 'price')) {
            return;
        }

        Schema::table('plans', function (Blueprint $table) {
            $table->decimal('price', 10, 2)->default(0)->after('name');
        });
    }

    public function down(): void
    {
        Schema::table('plans', function (Blueprint $table) {
            $table->dropColumn('price');
        });
    }
};
