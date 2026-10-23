<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 上游子用户 = 平台用户拿到的独立代理凭据。
 *
 * 一行对应 Webshare 的一个 subuser。user_id 为空表示「已开号但还没分配给平台用户」。
 * password 用 encrypted cast 加密存储，并加入 $hidden，接口不回明文（停用后密码已改废）。
 * (provider_id, upstream_id) 唯一：同一个 Webshare 子用户不会被重复落库。
 */
return new class extends Migration
{
    public function up(): void
    {
        // 表已存在（备份导入 / 重复执行）→ 跳过，避免 1050 撞车
        if (Schema::hasTable('proxy_accounts')) {
            return;
        }

        Schema::create('proxy_accounts', function (Blueprint $table) {
            $table->id();
            $table->foreignId('provider_id')->constrained('proxy_providers')->cascadeOnDelete();
            $table->string('upstream_id', 64)->comment('Webshare subuser id（字符串化，防供应商改类型）');
            $table->foreignId('user_id')->nullable()->constrained('users')->nullOnDelete()
                ->comment('分配给的平台用户；空=未分配');
            $table->string('label', 64)->comment('子用户备注（Webshare 侧 label）');
            $table->string('username', 64)->comment('代理用户名');
            $table->text('password')->nullable()->comment('代理密码（encrypted cast 加密存储）');
            // 单位 GB；0 = 不限量。停用时降到 0.01（Webshare 没有启用/禁用开关）
            $table->decimal('proxy_limit', 8, 2)->default(0)->comment('带宽上限 GB，0=不限量');
            $table->unsignedInteger('max_thread_count')->default(0)->comment('并发线程上限');
            $table->json('proxy_countries')->nullable()->comment('国家池 {"US":500,...}');
            $table->unsignedBigInteger('used_bytes')->default(0)->comment('已用流量（字节，上游回采）');
            // active/disabled/deleted
            $table->string('status', 16)->default('active')->comment('active/disabled');
            $table->timestamp('last_synced_at')->nullable()->comment('最近一次用量回采时间');
            $table->timestamps();

            $table->unique(['provider_id', 'upstream_id']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('proxy_accounts');
    }
};
