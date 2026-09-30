<?php

use App\Models\SiteConfig;
use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 节点流量倍率：原始倍率（site_configs）+ 节点覆盖（nodes.traffic_multiplier）。
 *
 * 两个概念不要混：
 *   - 原始倍率 default_node_multiplier：默认值，所有「继承中」的节点用它
 *   - 节点倍率 nodes.traffic_multiplier：可空。NULL = 继承原始倍率；有值 = 手动指定，优先
 *
 * 改原始倍率只影响继承中的节点，手动指定的节点永远不受影响（不是批量覆盖操作）。
 *
 * 老节点全是 NULL = 继承 = 1.0，行为与改动前完全一致，不动任何已有数据。
 */
return new class extends Migration
{
    public function up(): void
    {
        // 逐列守卫（照 2026_06_28_000200 的写法）：备份 dump 带来的老 nodes 表可能已经
        // 有这列但没有迁移记录，不守卫就是 1060 重复列。
        Schema::table('nodes', function (Blueprint $table) {
            if (! Schema::hasColumn('nodes', 'traffic_multiplier')) {
                $table->decimal('traffic_multiplier', 6, 2)->nullable()->default(null)->after('status');
            }
        });

        // firstOrCreate 而不是 updateOrCreate：用户已经改过的原始倍率不能被迁移覆盖回 1.0。
        // show_node_multiplier 默认「关」= 不建记录，用户端 Api\NodeController 见到没有这行
        // 就不返回 traffic_multiplier 字段，行为与改动前完全一致。
        SiteConfig::firstOrCreate(['key' => 'default_node_multiplier'], ['value' => '1.0']);
    }

    public function down(): void
    {
        Schema::table('nodes', function (Blueprint $table) {
            $table->dropColumn('traffic_multiplier');
        });
    }
};
