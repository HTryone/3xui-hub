<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        Schema::create('operation_logs', function (Blueprint $table) {
            $table->id();
            $table->string('type', 20);              // admin | user | system
            $table->unsignedBigInteger('actor_id')->nullable();
            $table->string('actor_name')->nullable(); // 邮箱/用户名
            $table->string('action', 120);           // 动作描述
            $table->string('method', 10)->nullable();
            $table->string('path', 255)->nullable();
            $table->integer('status')->nullable();    // HTTP 状态码
            $table->text('error')->nullable();        // 报错信息
            $table->string('ip', 45)->nullable();
            $table->timestamp('created_at')->nullable();
            $table->index(['type', 'created_at']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('operation_logs');
    }
};
