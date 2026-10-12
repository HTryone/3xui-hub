<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * xray_balancers 表：xray 节点路由负载均衡器（按序渲染进 routing.balancers）。
 *
 * 语义（与 xray routing.balancer 对齐）：
 * - selector：出站 tag 前缀数组（xray 前缀匹配；一个 selector 可命中多个出站）
 * - strategy：random / roundRobin / leastPing / leastLoad（default roundRobin）
 * - fallback_tag：目标出站不可用时回退的出站 tag（可空）
 * - enabled：停用即不渲染、也不参与出站候选
 *
 * 出站 tag 候选（路由目标 + balancer）在本表与 xray_routing_rules 之间共享，
 * 校验见 XrayRoutingController。基础设置（domainStrategy / domainMatcher）
 * 存 nodes.driver_config.xray_routing，无需建表。
 */
return new class extends Migration
{
    public function up(): void
    {
        // 幂等：表已存在（备份导入 / 重复执行）→ 跳过，避免 1050 table exists
        if (Schema::hasTable('xray_balancers')) {
            return;
        }

        Schema::create('xray_balancers', function (Blueprint $table) {
            $table->id();
            $table->foreignId('node_id')->constrained()->cascadeOnDelete();
            $table->string('tag', 64);                          // 同节点唯一，路由规则按 tag 引用
            $table->json('selectors');                          // ["proxy-","hk-"] 前缀数组
            $table->string('strategy', 16)->default('roundRobin'); // random|roundRobin|leastPing|leastLoad
            $table->string('fallback_tag', 64)->nullable();     // 回退出站 tag
            $table->string('remark', 128)->nullable();
            $table->boolean('enabled')->default(true);
            $table->unsignedInteger('sort')->default(0);
            $table->timestamps();

            $table->unique(['node_id', 'tag']);
            $table->index(['node_id', 'enabled', 'sort']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('xray_balancers');
    }
};
