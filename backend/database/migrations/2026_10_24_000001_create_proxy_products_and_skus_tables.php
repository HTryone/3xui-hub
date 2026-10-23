<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 代理商城：商品 + 规格（SKU）。
 *
 * 为什么不复用 plans：plans 是节点/订阅套餐（周期/总量 + 流量 + 节点注入），
 * 代理是独立产品线（上游子账号、按国家、库存 = 还能开几个子用户），
 * 混在一张表会让两条业务互相污染。
 *
 * 库存三态与 dujiao 那套一致：
 *   stock_total   可售总数（= 上游还能开几个子用户，站长手动维护 / 对账刷新）
 *   stock_locked  已下单待支付的占用
 *   stock_sold    已支付成功、已开号的
 * 下单 reserve、支付 consume、超时/取消 release，靠条件更新防超卖。
 */
return new class extends Migration
{
    public function up(): void
    {
        if (! Schema::hasTable('proxy_products')) {
            Schema::create('proxy_products', function (Blueprint $table) {
                $table->id();
                $table->string('name', 100)->comment('商品名（对外展示）');
                $table->string('subtitle', 200)->nullable()->comment('一句话卖点');
                $table->text('description')->nullable()->comment('详情（支持纯文本/简单 Markdown）');
                $table->string('image_url', 500)->nullable()->comment('封面图地址');
                $table->boolean('is_active')->default(true)->comment('上架开关');
                $table->unsignedInteger('sort_order')->default(0)->comment('排序，小的在前');
                $table->timestamps();
                $table->index(['is_active', 'sort_order']);
            });
        }

        if (! Schema::hasTable('proxy_skus')) {
            Schema::create('proxy_skus', function (Blueprint $table) {
                $table->id();
                $table->unsignedBigInteger('product_id')->comment('所属商品');
                $table->string('name', 100)->comment('规格名，如 100G/30天');
                // 货源类型 + 分配方式：决定下单后怎么交付
                //   webshare + exclusive：开一个上游子账号，凭据只有本人能用（库存 = 上游剩余子账号数）
                //   socks     + shared   ：多人共用同一条 socks 账号（库存 = 手动填的份数）
                $table->string('source_type', 16)->default('webshare')
                    ->comment('货源类型：webshare / socks');
                $table->string('allocation', 16)->default('exclusive')
                    ->comment('分配方式：exclusive=独占账号 / shared=多人共用');
                $table->unsignedBigInteger('source_id')->nullable()
                    ->comment('关联货源：webshare=proxy_providers.id；socks=proxy_socks.id');
                $table->string('region', 32)->nullable()->comment('地区（US/GB/ZZ=不限），null = 不限');
                // 流量额度只对 webshare 有意义（建号时传给上游 proxy_limit）；
                // socks 共享型交给上游自己管，这里存 0 表示不限
                $table->unsignedInteger('traffic_gb')->default(0)->comment('流量额度（GB，0 = 不限）');
                $table->unsignedInteger('duration_days')->default(30)->comment('有效期天数');
                // 并发线程：0 = 用上游默认
                $table->unsignedInteger('max_thread_count')->default(0)->comment('并发线程数，0 = 上游默认');
                $table->decimal('price', 10, 2)->default(0)->comment('售价');
                $table->decimal('cost_price', 10, 2)->default(0)->comment('成本价（算毛利，不对外）');
                // 库存三态
                $table->unsignedInteger('stock_total')->default(0)->comment('可售总数（= 上游可开子用户数）');
                $table->unsignedInteger('stock_locked')->default(0)->comment('待支付占用');
                $table->unsignedInteger('stock_sold')->default(0)->comment('已售出');
                $table->boolean('is_active')->default(true)->comment('规格启用');
                $table->unsignedInteger('sort_order')->default(0)->comment('排序');
                $table->timestamps();
                $table->index(['product_id', 'is_active', 'sort_order']);
            });
        }
    }

    public function down(): void
    {
        Schema::dropIfExists('proxy_skus');
        Schema::dropIfExists('proxy_products');
    }
};
