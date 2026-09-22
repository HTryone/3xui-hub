<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * node_inbounds 唯一约束改为 (node_id, protocol, inbound_id)，支持多入站。
 */
return new class extends Migration
{
    public function up(): void
    {
        // 幂等：新唯一键已在 → 说明这条迁移的效果早就达成了，整段跳过。
        // 必须在这里就 return，不能在闭包里逐个判断：dump 把整套表带过来时（migrations 表里
        // 却没有这条记录），下面那些 dropForeign / dropUnique 会对着已经改好的表再动一遍 ——
        // 索引已经被上一次删掉了，重删只会 1091，等于把 migrate 卡死在这条上。
        if (Schema::hasIndex('node_inbounds', 'node_inbounds_node_id_protocol_inbound_id_unique')) {
            return;
        }

        Schema::table('node_inbounds', function (Blueprint $table) {
            // MySQL 不允许删被外键引用的索引，先删外键
            $table->dropForeign(['node_id']);
            $table->dropUnique(['node_id', 'protocol']);
            $table->unique(['node_id', 'protocol', 'inbound_id']);
            $table->foreign('node_id')->references('id')->on('nodes')->cascadeOnDelete();
        });
    }

    public function down(): void
    {
        Schema::table('node_inbounds', function (Blueprint $table) {
            $table->dropForeign(['node_id']);
            $table->dropUnique(['node_id', 'protocol', 'inbound_id']);
            $table->unique(['node_id', 'protocol']);
            $table->foreign('node_id')->references('id')->on('nodes')->cascadeOnDelete();
        });
    }
};
