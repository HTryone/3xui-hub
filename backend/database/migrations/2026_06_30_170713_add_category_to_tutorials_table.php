<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        // 幂等：列已存在（备份导入 / 重复执行）→ 跳过，避免 1060 duplicate column
        if (Schema::hasColumn('tutorials', 'category')) {
            return;
        }

        Schema::table('tutorials', function (Blueprint $table) {
            $table->string('category')->default('默认')->after('title');
        });
    }

    public function down(): void
    {
        Schema::table('tutorials', function (Blueprint $table) {
            $table->dropColumn('category');
        });
    }
};
