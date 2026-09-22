<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * users 表添加 password 字段，支持邮箱登录。
 */
return new class extends Migration
{
    public function up(): void
    {
        // 幂等：列已存在（备份导入 / 重复执行）→ 跳过，避免 1060 duplicate column
        if (Schema::hasColumn('users', 'password')) {
            return;
        }

        Schema::table('users', function (Blueprint $table) {
            $table->string('password')->nullable()->after('email');
        });
    }

    public function down(): void
    {
        Schema::table('users', function (Blueprint $table) {
            $table->dropColumn('password');
        });
    }
};
