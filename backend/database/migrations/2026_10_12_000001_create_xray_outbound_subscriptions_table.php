<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * xray_outbound_subscriptions 表：出站订阅（服务器池，per-node）。
 *
 * 管理员接入一条外部订阅 URL，后台定时拉取分享链接 → 解析成出站 → 按稳定
 * tag 注入该节点的 config（renderOutbounds 合并）。标签稳定是核心：同一台
 * 服务器跨刷新保持同一 tag，路由/负载均衡引用才不会被刷新打断。
 *
 * 字段口径（对齐 3x-ui outbound_subscription）：
 * - url：订阅地址（仅 http/https，SSRF 防护在服务层）
 * - tag_prefix：出站 tag 前缀（留空回退 sub<id>-）
 * - update_interval：刷新间隔（秒，默认 600）
 * - prepend：注入位置（true=排在自定义出站之前，false=之后）
 * - priority：同组内排序（小者靠前）
 * - fetched：最近一次成功拉取解析出的出站 JSON（缓存，渲染直接读它；为空则不注入）
 * - link_identities：identity → tag 映射（下次刷新据此复用 tag，保稳定）
 * - content_hash：最近一次成功拉取的内容指纹（内容变化才 bump config_version）
 */
return new class extends Migration
{
    public function up(): void
    {
        // 表已存在（备份导入 / 重复执行）→ 跳过，避免 1050 撞车
        if (Schema::hasTable('xray_outbound_subscriptions')) {
            return;
        }

        Schema::create('xray_outbound_subscriptions', function (Blueprint $table) {
            $table->id();
            $table->foreignId('node_id')->constrained()->cascadeOnDelete();
            $table->string('remark', 128)->nullable()->comment('管理员备注名');
            $table->text('url')->comment('订阅地址（http/https）');
            $table->string('tag_prefix', 64)->nullable()->comment('出站 tag 前缀；留空回退 sub<id>-');
            $table->unsignedInteger('update_interval')->default(600)->comment('刷新间隔（秒）');
            $table->boolean('prepend')->default(false)->comment('注入位置：true=排在自定义出站之前');
            $table->unsignedInteger('priority')->default(0)->comment('同组内排序（小者靠前）');
            $table->boolean('enabled')->default(true);
            // 拉取缓存：订阅可聚合很多出站，用 mediumText 兜底（同 third_party_subs.cached_links 口径）
            $table->mediumText('fetched')->nullable()->comment('最近一次成功拉取的出站 JSON 数组');
            $table->mediumText('link_identities')->nullable()->comment('identity → tag 映射（保 tag 稳定）');
            $table->string('content_hash', 64)->nullable()->comment('最近一次成功拉取的内容指纹');
            $table->unsignedInteger('node_count')->default(0)->comment('最近一次拉取到的出站数');
            $table->timestamp('last_fetched_at')->nullable()->comment('最近一次拉取时间');
            $table->string('last_status', 16)->default('pending')->comment('pending/success/failed');
            $table->string('last_error', 500)->nullable()->comment('最近一次拉取失败原因');
            $table->timestamps();

            $table->index(['node_id', 'enabled']);
            $table->index(['enabled', 'priority']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('xray_outbound_subscriptions');
    }
};
