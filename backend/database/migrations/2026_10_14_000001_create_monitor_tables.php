<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 机器流量监控的两张表（L0 原始采样 + 天聚合）。
 *
 * 背景：面板 6 台节点里，5 台 3x-ui 的机器累计收发字节随每 5 分钟的健康检查
 * （/panel/api/server/status 的 netTraffic）已经拿到、却被当场丢弃；1 台 xray 裸节点
 * 由 agent 上报 net_total_up/down（存在 driver_config.agent）。采购/扩容要看的是
 * 「合计流量 / TOP 排行 / 趋势曲线」，所以唯一缺的是**历史采样与查询**。
 *
 * monitor_samples（L0，5 分钟粒度，保留 config('monitor.retain_days') 天，默认 90）：
 *   sent / recv 是机器网卡的**累计值**（不是增量），差分口径在上层
 *   （MonitorService::sumDeltas / rollup），这样任何时点的口径都由同一条规则推出来。
 *
 * monitor_daily（天聚合，永久）：rollup 把过期样本折成「当天增量」后落在这里，
 *   原始样本删掉但结论永久保留 —— 采样表不随时间无界增长。
 */
return new class extends Migration
{
    public function up(): void
    {
        // 逐表守卫（照 xray_outbound_subscriptions 的写法）：备份导入 / 重复执行时
        // 表可能已经在，不守卫就是 1050 撞车，会把排在后面的迁移全卡住。
        if (! Schema::hasTable('monitor_samples')) {
            Schema::create('monitor_samples', function (Blueprint $table) {
                $table->id();
                $table->foreignId('node_id')->constrained()->cascadeOnDelete();
                // 累计收发字节：机器网卡从开机起的累计值，不是区间增量（差分在上层算）。
                // 累计值可能超过 4G，必须 unsignedBigInteger。
                $table->unsignedBigInteger('sent')->default(0)->comment('累计发送字节（netTraffic.sent / agent net_total_up）');
                $table->unsignedBigInteger('recv')->default(0)->comment('累计接收字节（netTraffic.recv / agent net_total_down）');
                // cpu/mem 可空：xray 老 agent 不上报 → null（无数据，不是 0）
                $table->decimal('cpu', 5, 2)->nullable()->default(null);
                $table->decimal('mem', 5, 2)->nullable()->default(null);
                $table->datetime('sampled_at');
                $table->timestamps();

                // 查询主路径：按节点取时间窗口（summary / series）+ rollup 的过期扫描
                $table->index(['node_id', 'sampled_at']);
                $table->index('sampled_at');
            });
        }

        if (! Schema::hasTable('monitor_daily')) {
            Schema::create('monitor_daily', function (Blueprint $table) {
                $table->id();
                $table->foreignId('node_id')->constrained()->cascadeOnDelete();
                $table->date('date')->comment('站点时区下的自然日');
                $table->unsignedBigInteger('sent_delta')->default(0);
                $table->unsignedBigInteger('recv_delta')->default(0);
                $table->timestamps();

                // rollup 按 (node_id, date) upsert（覆盖而非累加）—— 唯一键保证幂等
                $table->unique(['node_id', 'date']);
            });
        }
    }

    public function down(): void
    {
        Schema::dropIfExists('monitor_daily');
        Schema::dropIfExists('monitor_samples');
    }
};
