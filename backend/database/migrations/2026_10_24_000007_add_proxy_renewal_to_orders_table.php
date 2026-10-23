<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 代理订单区分「新开通」与「续费」。
 *
 * 新开通才占用 SKU 库存（下单 reserve、支付 consume）；
 * 续费只是延长同一个凭据的到期时间，不消耗库存 —— 这张标记让支付回调
 * 不必反推「下单时有没有占用过」，避免并发下算错库存。
 */
return new class extends Migration
{
    public function up(): void
    {
        if (Schema::hasColumn('orders', 'proxy_renewal')) {
            return;
        }

        Schema::table('orders', function (Blueprint $table) {
            $table->boolean('proxy_renewal')->default(false)->after('proxy_sku_id')
                ->comment('代理订单：true=续费（不占库存），false=新开通');
        });
    }

    public function down(): void
    {
        if (Schema::hasColumn('orders', 'proxy_renewal')) {
            Schema::table('orders', function (Blueprint $table) {
                $table->dropColumn('proxy_renewal');
            });
        }
    }
};
