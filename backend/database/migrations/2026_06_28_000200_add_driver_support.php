<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        // 这条迁移给 5 张表加了 11 列 + 1 个索引，是「备份导入后重跑」最容易撞 1060/1061 的一条。
        // 全部逐列 / 逐索引判断：已存在的跳过。整段提前 return 不行 —— dump 带来的老表可能只
        // 加了其中一部分（例如 users 有 driver_identifier 却没有 client_config）。
        // 闭包里一个命令都没加时 Laravel 不会下发任何 SQL，所以全齐了就是空操作。

        // 1. nodes 表：加 driver 标识
        Schema::table('nodes', function (Blueprint $table) {
            if (! Schema::hasColumn('nodes', 'driver_type')) {
                $table->string('driver_type', 32)->default('3x-ui')->after('enabled');
            }
            if (! Schema::hasColumn('nodes', 'driver_version')) {
                $table->string('driver_version', 16)->nullable()->after('driver_type');
            }
            if (! Schema::hasColumn('nodes', 'driver_config')) {
                $table->json('driver_config')->nullable()->after('driver_version');
            }
            if (! Schema::hasIndex('nodes', 'nodes_driver_type_enabled_index')) {
                $table->index(['driver_type', 'enabled']);
            }
        });

        // 2. users 表： driver 专属标识
        Schema::table('users', function (Blueprint $table) {
            if (! Schema::hasColumn('users', 'driver_identifier')) {
                $table->string('driver_identifier', 128)->nullable()->after('uuid');
            }
            if (! Schema::hasColumn('users', 'client_config')) {
                $table->json('client_config')->nullable()->after('driver_identifier');
            }
        });

        // 3. node_inbounds 表
        Schema::table('node_inbounds', function (Blueprint $table) {
            if (! Schema::hasColumn('node_inbounds', 'driver_type')) {
                $table->string('driver_type', 32)->default('3x-ui')->after('protocol');
            }
        });

        // 4. payment_configs 表：从硬编码字段 → JSON 配置
        Schema::table('payment_configs', function (Blueprint $table) {
            if (! Schema::hasColumn('payment_configs', 'driver_type')) {
                $table->string('driver_type', 32)->default('wwspay')->after('name');
            }
            if (! Schema::hasColumn('payment_configs', 'driver_config')) {
                $table->json('driver_config')->nullable()->after('driver_type');
            }
        });

        // 5. orders 表
        Schema::table('orders', function (Blueprint $table) {
            if (! Schema::hasColumn('orders', 'currency')) {
                $table->string('currency', 8)->default('CNY')->after('amount');
            }
            if (! Schema::hasColumn('orders', 'payment_driver_type')) {
                $table->string('payment_driver_type', 32)->nullable()->after('payment_config_id');
            }
            if (! Schema::hasColumn('orders', 'payment_metadata')) {
                $table->json('payment_metadata')->nullable()->after('payment_driver_type');
            }
        });
    }

    public function down(): void
    {
        Schema::table('nodes', function (Blueprint $table) {
            $table->dropIndex(['driver_type', 'enabled']);
            $table->dropColumn(['driver_type', 'driver_version', 'driver_config']);
        });

        Schema::table('users', function (Blueprint $table) {
            $table->dropColumn(['driver_identifier', 'client_config']);
        });

        Schema::table('node_inbounds', function (Blueprint $table) {
            $table->dropColumn('driver_type');
        });

        Schema::table('payment_configs', function (Blueprint $table) {
            $table->dropColumn(['driver_type', 'driver_config']);
        });

        Schema::table('orders', function (Blueprint $table) {
            $table->dropColumn(['currency', 'payment_driver_type', 'payment_metadata']);
        });
    }
};
