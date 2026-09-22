<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        // 幂等：两列逐列判断（备份导入 / 重复执行）→ 已存在的列跳过，避免 1060
        Schema::table('admins', function (Blueprint $table) {
            if (! Schema::hasColumn('admins', 'google2fa_secret')) {
                $table->string('google2fa_secret')->nullable()->after('password');
            }

            if (! Schema::hasColumn('admins', 'google2fa_enabled')) {
                $table->boolean('google2fa_enabled')->default(false)->after('google2fa_secret');
            }
        });
    }

    public function down(): void
    {
        Schema::table('admins', function (Blueprint $table) {
            $table->dropColumn(['google2fa_secret', 'google2fa_enabled']);
        });
    }
};
