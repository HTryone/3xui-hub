<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * xray_outbounds 增加中转目标定位列：记录该出站由哪条「中转」生成（目标节点 + 目标入站）。
 *
 * 用途：路由目标选「其它机器的入站」时自动生成的中转出站，重复保存同一目标要复用
 * 同一条出站（幂等），删除规则时也能据此回收。手工出站两列为 NULL。
 *
 * 目标入站 id 语义随目标节点类型区分：
 *   - xray 目标：xray_inbounds.id
 *   - 3x-ui 目标：node_inbounds.inbound_id
 * 用 target_node_id 区分，故不建外键（跨表弱引用）。
 */
return new class extends Migration
{
    public function up(): void
    {
        Schema::table('xray_outbounds', function (Blueprint $table) {
            if (! Schema::hasColumn('xray_outbounds', 'relay_target_node_id')) {
                $table->unsignedBigInteger('relay_target_node_id')->nullable()->after('remark');
            }
            if (! Schema::hasColumn('xray_outbounds', 'relay_target_inbound_id')) {
                $table->unsignedBigInteger('relay_target_inbound_id')->nullable()->after('relay_target_node_id');
            }
        });
    }

    public function down(): void
    {
        Schema::table('xray_outbounds', function (Blueprint $table) {
            $table->dropColumn(['relay_target_node_id', 'relay_target_inbound_id']);
        });
    }
};
