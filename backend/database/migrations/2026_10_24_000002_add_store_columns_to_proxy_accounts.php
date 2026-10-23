<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 代理账号补「售卖维度」字段：买的是哪个 SKU、给了多少流量、什么时候到期、首单是哪笔。
 *
 * 语义：
 *   - sku_id / traffic_limit_bytes / expires_at 只在「商城购买」路径写入；
 *     管理员手动开的号这三列为空（不受商城到期/超额任务影响）
 *   - order_id 指向首次开通的那笔订单，便于对账；续费不覆盖它
 */
return new class extends Migration
{
    public function up(): void
    {
        if (! Schema::hasColumn('proxy_accounts', 'sku_id')) {
            Schema::table('proxy_accounts', function (Blueprint $table) {
                $table->unsignedBigInteger('sku_id')->nullable()->after('user_id')
                    ->comment('购买的代理规格（手动开的号为空）');
                $table->unsignedBigInteger('traffic_limit_bytes')->nullable()->after('proxy_limit')
                    ->comment('本账号流量上限（字节，= SKU 的 traffic_gb）');
                $table->timestamp('expires_at')->nullable()->after('traffic_limit_bytes')
                    ->comment('到期时间（手动开的号为空 = 不过期）');
                $table->unsignedBigInteger('order_id')->nullable()->after('expires_at')
                    ->comment('首次开通订单 id（续费不覆盖）');
                $table->index(['user_id', 'status']);
            });
        }
    }

    public function down(): void
    {
        if (Schema::hasColumn('proxy_accounts', 'sku_id')) {
            Schema::table('proxy_accounts', function (Blueprint $table) {
                $table->dropIndex(['user_id', 'status']);
                $table->dropColumn(['sku_id', 'traffic_limit_bytes', 'expires_at', 'order_id']);
            });
        }
    }
};
