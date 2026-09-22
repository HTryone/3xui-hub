<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        // 表已存在（例如从备份导入，或表由其它途径建过）→ 跳过创建，避免 1050 撞车
        if (Schema::hasTable('async_tasks')) {
            return;
        }

        Schema::create('async_tasks', function (Blueprint $table) {
            $table->id();
            $table->string('type');
            $table->string('status')->default('pending')->index();
            $table->string('subject_type')->nullable();
            $table->unsignedBigInteger('subject_id')->nullable();
            $table->unsignedInteger('total')->default(0);
            $table->unsignedInteger('completed')->default(0);
            $table->unsignedInteger('failed')->default(0);
            $table->unsignedInteger('attempts')->default(0);
            $table->unsignedInteger('max_attempts')->default(3);
            $table->text('error')->nullable();
            $table->json('meta')->nullable();
            $table->timestamp('started_at')->nullable();
            $table->timestamp('finished_at')->nullable();
            $table->timestamps();

            $table->index(['subject_type', 'subject_id']);
            $table->index(['type', 'created_at']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('async_tasks');
    }
};