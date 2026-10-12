<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * xray_relay_clients 表：中转时面板在**目标** xray 节点入站上自动生成的 relay 凭据。
 *
 * 场景：源节点路由目标选「其它机器的入站」→ 保存时
 *   1) 源节点建一条中转出站（xray_outbounds，tag=rl-{n}-{port}）
 *   2) 目标节点入站需要一份对应凭据（本表记录），渲染进该入站 clients
 *
 * 一行 = 某个「源节点 × 目标入站」组合的一对凭据：
 *   - 目标凭据（id/password）给源节点出站连接用
 *   - source_node_id + source_inbound_id 记录来源，供删除/重装时回收
 * - id 明文存（uuid / trojan password）—— 面板要把它渲染进目标入站 clients，
 *   非法加密（与 xray_inbounds.reality_private_key 加密不同，这里非机密私钥）
 */
return new class extends Migration
{
    public function up(): void
    {
        // 幂等：表已存在（备份导入 / 重复执行）→ 跳过，避免 1050 table exists
        if (Schema::hasTable('xray_relay_clients')) {
            return;
        }

        Schema::create('xray_relay_clients', function (Blueprint $table) {
            $table->id();
            $table->foreignId('node_id')->constrained()->cascadeOnDelete();       // 目标节点
            $table->foreignId('inbound_id')->constrained('xray_inbounds')->cascadeOnDelete(); // 目标入站
            $table->string('email', 128);             // client email（唯一，便于回收）
            $table->string('client_id', 255);         // vless/vmess uuid 或 trojan password
            $table->foreignId('source_node_id')->nullable()->constrained('nodes')->nullOnDelete(); // 来源节点
            $table->unsignedBigInteger('source_inbound_id')->nullable(); // 来源入站（xray_inbounds.id，弱引用）
            $table->timestamps();

            $table->unique(['node_id', 'email']);
            $table->unique(['node_id', 'inbound_id', 'client_id']); // 同入站不重复注入同一凭据
            $table->index(['source_node_id', 'source_inbound_id']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('xray_relay_clients');
    }
};
