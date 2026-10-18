<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 用户×节点 按小时流量表（user_traffic_hourly）。
 *
 * 存在理由只有一个：「最近 24 小时」是滑动窗口，天表按自然日分桶做不到。
 * 定位与 monitor_samples 相同：细粒度 L0，只保留较短时间
 * （config('user_traffic.hourly_retain_days')，默认 7 天），过期在同步路径上懒删除。
 * 天表每轮同步实时累加，从上线第一天起完整，不依赖小时表汇总。
 *
 * hour_start 存应用时区（UTC）的小时起点，与 monitor_samples.sampled_at 同口径；
 * 天表的 date 存业务时区自然日，与 monitor_daily.date 同口径。
 */
return new class extends Migration
{
    public function up(): void
    {
        // 逐表守卫：备份 dump 会带表但不带 migrations 记录，不守卫就是 1050 撞车。
        if (Schema::hasTable('user_traffic_hourly')) {
            return;
        }

        Schema::create('user_traffic_hourly', function (Blueprint $table) {
            $table->id();
            $table->foreignId('user_id')->constrained()->cascadeOnDelete();
            $table->foreignId('node_id')->constrained()->cascadeOnDelete();
            // 应用时区（UTC）的小时起点。datetime 而非 timestamp：与 monitor_samples 一致，
            // 不受 2038 限制，且写入端直接给字符串，不经时区二次转换。
            $table->datetime('hour_start');
            // 三个增量列都是「区间增量」不是累计值；累计字节可超 4G，必须 unsignedBigInteger。
            $table->unsignedBigInteger('up_delta')->default(0)->comment('面板侧原始上行字节（未乘倍率）');
            $table->unsignedBigInteger('down_delta')->default(0)->comment('面板侧原始下行字节（未乘倍率）');
            $table->unsignedBigInteger('weighted_delta')->default(0)->comment('乘节点倍率后的字节（= 套餐扣费口径）');
            $table->timestamps();

            // 累加型 upsert 的冲突键
            $table->unique(['user_id', 'node_id', 'hour_start'], 'user_traffic_hourly_unique');
            // 24h 按用户排行 / 按机器排行各一条覆盖索引
            $table->index(['hour_start', 'user_id']);
            $table->index(['hour_start', 'node_id']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('user_traffic_hourly');
    }
};
