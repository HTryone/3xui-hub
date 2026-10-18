<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 用户×节点 按天流量表（user_traffic_daily）。
 *
 * date 是业务时区（config('panel.business_timezone')）的自然日，与 monitor_daily.date 同口径。
 * 与 user_traffic_hourly 的分工：本表永久保留、每轮同步实时累加（不等小时表过期），
 * 服务 7 天 / 30 天窗口；小时表只服务 24 小时滑动窗口、保留 7 天。
 */
return new class extends Migration
{
    public function up(): void
    {
        // 逐表守卫：备份 dump 会带表但不带 migrations 记录，不守卫就是 1050 撞车。
        if (Schema::hasTable('user_traffic_daily')) {
            return;
        }

        Schema::create('user_traffic_daily', function (Blueprint $table) {
            $table->id();
            $table->foreignId('user_id')->constrained()->cascadeOnDelete();
            $table->foreignId('node_id')->constrained()->cascadeOnDelete();
            // 业务时区自然日（DATE，不带时间）
            $table->date('date');
            $table->unsignedBigInteger('up_delta')->default(0)->comment('面板侧原始上行字节（未乘倍率）');
            $table->unsignedBigInteger('down_delta')->default(0)->comment('面板侧原始下行字节（未乘倍率）');
            $table->unsignedBigInteger('weighted_delta')->default(0)->comment('乘节点倍率后的字节（= 套餐扣费口径）');
            $table->timestamps();

            $table->unique(['user_id', 'node_id', 'date']);
            $table->index(['date', 'user_id']);
            $table->index(['date', 'node_id']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('user_traffic_daily');
    }
};
