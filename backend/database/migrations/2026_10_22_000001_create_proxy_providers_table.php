<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 代理上游账号（站长自己的 Webshare 账号，可配多个）。
 *
 * api_token 是 Webshare 的 API Key（Authorization: Token <key>），属敏感信息，
 * 模型侧用 encrypted cast 加密存储，接口只回「是否已配置」，不回明文。
 */
return new class extends Migration
{
    public function up(): void
    {
        // 表已存在（备份导入 / 重复执行）→ 跳过，避免 1050 撞车
        if (Schema::hasTable('proxy_providers')) {
            return;
        }

        Schema::create('proxy_providers', function (Blueprint $table) {
            $table->id();
            $table->string('name', 64)->comment('管理员备注名（仅后台可见）');
            $table->string('driver', 32)->default('webshare')->comment('上游类型，当前只有 webshare');
            $table->text('api_token')->comment('上游 API Token（encrypted cast 加密存储）');
            $table->boolean('enabled')->default(true)->comment('是否参与开号/同步');
            // 最近一次连通性检查结果：pending/success/failed
            $table->string('last_status', 16)->default('pending')->comment('pending/success/failed');
            $table->string('last_error', 255)->nullable()->comment('最近一次失败原因');
            $table->timestamp('last_checked_at')->nullable()->comment('最近一次检查时间');
            $table->timestamps();
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('proxy_providers');
    }
};
