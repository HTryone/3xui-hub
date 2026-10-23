<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 优惠码文件夹分组。
 *
 * - 新表 discount_code_folders：只存 id / name，码与文件夹的从属关系落在 discount_codes.folder_id
 * - discount_codes.folder_id 可空，null = 未归档（老数据全部保持 null，不做任何回填）
 *
 * 删除文件夹时把里面码的 folder_id 置 null（不删码），由 DiscountCodeFolderController 负责；
 * 这里不建外键，与 user_id / plan_id 的既有约定一致（备份导入不按表顺序走）。
 */
return new class extends Migration
{
    public function up(): void
    {
        // 幂等：备份导入自带表结构但不带 migrations 记录，重复执行会撞 1050
        if (! Schema::hasTable('discount_code_folders')) {
            Schema::create('discount_code_folders', function (Blueprint $table) {
                $table->id();
                $table->string('name', 60)->comment('文件夹名');
                $table->timestamps();
            });
        }

        // 幂等：列已存在（重复执行 / 备份里已经带了这一列）时不再重复添加
        if (! Schema::hasColumn('discount_codes', 'folder_id')) {
            Schema::table('discount_codes', function (Blueprint $table) {
                $table->unsignedBigInteger('folder_id')->nullable()->after('plan_id')
                    ->comment('所属文件夹 id，null = 未归档');
                $table->index('folder_id');
            });
        }
    }

    public function down(): void
    {
        if (Schema::hasColumn('discount_codes', 'folder_id')) {
            Schema::table('discount_codes', function (Blueprint $table) {
                $table->dropIndex(['folder_id']);
                $table->dropColumn('folder_id');
            });
        }

        Schema::dropIfExists('discount_code_folders');
    }
};
