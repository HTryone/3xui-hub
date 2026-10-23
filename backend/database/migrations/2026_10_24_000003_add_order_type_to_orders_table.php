<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 订单支持两类商品：节点套餐（原有）与代理套餐（新增）。
 *
 * 口径：
 *   - order_type 默认 'plan'，**老订单一行不用改**，支付回调仍走原逻辑
 *   - 'proxy' 走代理开通分支（开上游子账号 + 绑用户 + 记到期）
 *   - proxy_sku_id 只在 order_type='proxy' 时有值
 *   - plan_id 改为可空（代理订单没有节点套餐）
 */
return new class extends Migration
{
    public function up(): void
    {
        if (! Schema::hasColumn('orders', 'order_type')) {
            Schema::table('orders', function (Blueprint $table) {
                $table->string('order_type', 16)->default('plan')->after('user_id')
                    ->comment("订单类型：plan=节点套餐 / proxy=代理套餐");
                $table->unsignedBigInteger('proxy_sku_id')->nullable()->after('plan_id')
                    ->comment('代理规格 id（仅代理订单）');
                $table->index(['order_type', 'status']);
            });
        }

        // plan_id 改为可空：代理订单没有节点套餐
        if (Schema::hasColumn('orders', 'plan_id')) {
            Schema::table('orders', function (Blueprint $table) {
                $table->unsignedBigInteger('plan_id')->nullable()->change();
            });
        }
    }

    public function down(): void
    {
        if (Schema::hasColumn('orders', 'order_type')) {
            Schema::table('orders', function (Blueprint $table) {
                $table->dropIndex(['order_type', 'status']);
                $table->dropColumn(['order_type', 'proxy_sku_id']);
            });
        }
    }
};
