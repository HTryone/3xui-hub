<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * xray_inbounds 增加备注列：给入站一个可读的中文名（订阅节点名展示用）。
 *
 * tag 是引用型标识（xray 路由 / 删用户 / 中转拼接都按它定位），必须保持
 * [A-Za-z0-9_-]；想写中文只能填 remark。remark 为空时订阅名仍回落 tag，
 * 老入站不填 remark → 生成的订阅链接字节不变。
 */
return new class extends Migration
{
    public function up(): void
    {
        // 逐列守卫（照 2026_09_30_000001 的写法）：备份 dump 带来的老 xray_inbounds
        // 表可能已经有这列但没有迁移记录，不守卫就是 1060 重复列。
        Schema::table('xray_inbounds', function (Blueprint $table) {
            if (! Schema::hasColumn('xray_inbounds', 'remark')) {
                $table->string('remark', 128)->nullable()->after('tag');
            }
        });
    }

    public function down(): void
    {
        Schema::table('xray_inbounds', function (Blueprint $table) {
            $table->dropColumn('remark');
        });
    }
};
