<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        Schema::create('async_task_items', function (Blueprint $table) {
            $table->id();
            $table->foreignId('task_id')->constrained('async_tasks')->cascadeOnDelete();
            $table->string('item_key');
            $table->string('status')->default('pending')->index();
            $table->unsignedInteger('attempts')->default(0);
            $table->text('error')->nullable();
            $table->timestamp('started_at')->nullable();
            $table->timestamp('finished_at')->nullable();
            $table->timestamps();

            $table->unique(['task_id', 'item_key']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('async_task_items');
    }
};