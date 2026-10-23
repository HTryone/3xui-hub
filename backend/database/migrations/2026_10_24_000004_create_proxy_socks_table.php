<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 手工 socks 货源（共享型）。
 *
 * 站长手上一些 socks（IP:端口 + 账号密码），没有 API 可管，只能手工录。
 * 一条 socks 记录 = 一个「共享套餐」的载体：把它挂到某个 SKU 上，
 * 那个 SKU 就能卖给多个人共用同一套凭据（库存 = SKU 的 stock_total 份数）。
 *
 * 流量我们不统计（交给 socks 机器/上游自己管），所以这里没有用量字段。
 * password 用 encrypted cast 加密存，列表接口不回。
 */
return new class extends Migration
{
    public function up(): void
    {
        if (Schema::hasTable('proxy_socks')) {
            return;
        }

        Schema::create('proxy_socks', function (Blueprint $table) {
            $table->id();
            $table->string('name', 100)->comment('备注名，如 美国住宅 socks 池');
            $table->string('host', 190)->comment('地址（IP 或域名）');
            $table->unsignedInteger('port')->comment('端口');
            $table->string('username', 190)->nullable()->comment('认证账号（无认证留空）');
            $table->text('password')->nullable()->comment('认证密码（encrypted cast）');
            $table->string('country', 16)->nullable()->comment('国家代码，如 US');
            $table->string('city', 64)->nullable()->comment('城市');
            $table->string('note', 255)->nullable()->comment('备注');
            $table->boolean('is_active')->default(true)->comment('启用');
            $table->timestamps();
            $table->index(['is_active']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('proxy_socks');
    }
};
