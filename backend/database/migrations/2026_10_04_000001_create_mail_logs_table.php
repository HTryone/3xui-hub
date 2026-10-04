<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 邮件发送日志：记录自动通知与批量发信每一封的投递结果。
 *
 * type 区分来源：notify=自动通知，batch=批量发信，test=测试邮件。
 * status: sent=成功，failed=失败。
 * 批量发信上千封时靠这张表排查「到底哪几封没出去、为什么」。
 */
return new class extends Migration
{
    public function up(): void
    {
        Schema::create('mail_logs', function (Blueprint $table) {
            $table->id();
            $table->string('type', 20)->default('batch');          // notify | batch | test
            $table->string('status', 20)->default('sent');         // sent | failed
            $table->string('to_email')->nullable();                // 收件人
            $table->string('to_user_id')->nullable();              // 用户 ID（单发/通知时有）
            $table->string('subject')->nullable();                 // 邮件标题
            $table->text('error')->nullable();                     // 失败原因
            // 触发场景：traffic_exhausted / traffic_almost / expiring / expired / no_plan / manual
            $table->string('scene', 40)->nullable();
            $table->unsignedBigInteger('batch_id')->nullable();    // 同一次批量发信的关联 ID
            $table->timestamps();

            $table->index(['type', 'status']);
            $table->index('batch_id');
            $table->index('to_user_id');
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('mail_logs');
    }
};