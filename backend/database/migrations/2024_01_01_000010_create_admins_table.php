<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * ControlHub admins 表（system-design §5.1）。
 */
return new class extends Migration
{
    public function up(): void
    {
        // 表已存在（例如从备份导入，或表由其它途径建过）→ 跳过创建，避免 1050 撞车
        if (Schema::hasTable('admins')) {
            return;
        }

        Schema::create('admins', function (Blueprint $table) {
            $table->id();
            $table->string('username')->unique();
            $table->string('password'); // bcrypt
            $table->timestamps();
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('admins');
    }
};
