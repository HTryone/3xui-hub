<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        // 表已存在（例如从备份导入，或表由其它途径建过）→ 跳过创建，避免 1050 撞车
        if (Schema::hasTable('scheduled_task_runs')) {
            return;
        }

        Schema::create('scheduled_task_runs', function (Blueprint $table) {
            $table->id();
            $table->string('command', 255);
            $table->string('status', 20);   // finished | failed
            $table->text('error')->nullable();
            $table->unsignedInteger('duration_ms')->nullable();
            $table->timestamp('ran_at')->index();
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('scheduled_task_runs');
    }
};
