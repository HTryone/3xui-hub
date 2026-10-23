<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 用户代理权益（谁买了哪个代理套餐、什么时候到期、对应哪份凭据）。
 *
 * 为什么要单独一张表：
 *   - webshare 独占型：权益指向一条 proxy_accounts（一人一个子账号）
 *   - socks 共享型：多人共用同一条 proxy_socks，**不能**在 proxy_accounts 里一人一行
 *     （那条记录属于站长货源，不是用户资产）
 * 所以用权益表统一表达「用户买到了什么、到什么时候」，交付时按 kind 取对应凭据。
 *
 * 续费语义：同一用户 + 同一 kind 已有 active 权益时，不新建，只延长 expires_at。
 */
return new class extends Migration
{
    public function up(): void
    {
        if (Schema::hasTable('proxy_entitlements')) {
            return;
        }

        Schema::create('proxy_entitlements', function (Blueprint $table) {
            $table->id();
            $table->unsignedBigInteger('user_id')->comment('购买人');
            $table->unsignedBigInteger('order_id')->nullable()->comment('开通/续费的订单');
            $table->unsignedBigInteger('sku_id')->comment('购买的规格');
            $table->string('kind', 16)->comment('凭据类型：webshare=独占子账号 / socks=共享 socks');
            $table->unsignedBigInteger('account_id')->nullable()->comment('proxy_accounts.id（webshare）');
            $table->unsignedBigInteger('socks_id')->nullable()->comment('proxy_socks.id（共享）');
            $table->timestamp('expires_at')->nullable()->comment('到期时间');
            $table->string('status', 16)->default('active')->comment('active / expired / disabled');
            $table->timestamps();

            $table->index(['user_id', 'status']);
            $table->index(['status', 'expires_at']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('proxy_entitlements');
    }
};
