<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 给 proxy_skus 补「货源类型 / 分配方式 / 关联货源」三列。
 *
 * 为什么单独一个迁移：建表迁移 2026_10_24_000001 已经在部分环境跑过了
 * （改历史迁移的 body 不会重跑），这里用 hasColumn 幂等补列；
 * 全新安装时 000001 已带这三列，本迁移是空操作。
 *
 * 语义：
 *   webshare + exclusive → 开上游子账号，凭据独占（库存 = 上游剩余子账号数）
 *   socks     + shared   → 多人共用一条 socks 账号（库存 = 手填份数）
 */
return new class extends Migration
{
    public function up(): void
    {
        if (Schema::hasColumn('proxy_skus', 'source_type')) {
            return;
        }

        Schema::table('proxy_skus', function (Blueprint $table) {
            $table->string('source_type', 16)->default('webshare')->after('name')
                ->comment('货源类型：webshare / socks');
            $table->string('allocation', 16)->default('exclusive')->after('source_type')
                ->comment('分配方式：exclusive=独占账号 / shared=多人共用');
            $table->unsignedBigInteger('source_id')->nullable()->after('allocation')
                ->comment('关联货源：webshare=proxy_providers.id；socks=proxy_socks.id');
        });
    }

    public function down(): void
    {
        if (Schema::hasColumn('proxy_skus', 'source_type')) {
            Schema::table('proxy_skus', function (Blueprint $table) {
                $table->dropColumn(['source_type', 'allocation', 'source_id']);
            });
        }
    }
};
