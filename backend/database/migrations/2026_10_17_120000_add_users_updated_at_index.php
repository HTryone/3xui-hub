<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 配置指纹查询 users 的 MAX(updated_at)（xray 节点拉配置判 304 用），
 * 大用户量下必须走索引，否则每次拉取全表扫描。
 */
return new class extends Migration
{
    public function up(): void
    {
        Schema::table('users', function (Blueprint $table) {
            $table->index('updated_at', 'users_updated_at_index');
        });
    }

    public function down(): void
    {
        Schema::table('users', function (Blueprint $table) {
            $table->dropIndex('users_updated_at_index');
        });
    }
};
