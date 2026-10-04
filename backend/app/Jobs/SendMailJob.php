<?php

namespace App\Jobs;

use App\Models\MailLog;
use App\Models\User;
use App\Services\MailNotifyService;
use App\Services\SimpleMailerService;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;
use Illuminate\Support\Facades\Log;

/**
 * 发一封邮件并记录结果（自动通知 / 批量发信共用）。
 *
 * 每次投递都落一条 mail_logs：成功 sent，失败 failed + error。
 * 发信异常在此捕获，不让整个队列任务崩掉——批量发信里一封失败不该中断其余。
 */
class SendMailJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public function __construct(
        public string $toEmail,
        public string $subject,
        public string $htmlBody,
        public string $type = MailLog::TYPE_BATCH,
        public ?int $userId = null,
        public ?string $scene = null,
        public ?int $batchId = null,
    ) {
    }

    public function handle(SimpleMailerService $mailer): void
    {
        try {
            $mailer->send($this->toEmail, $this->subject, $this->htmlBody);

            MailLog::create([
                'type'       => $this->type,
                'status'     => MailLog::STATUS_SENT,
                'to_email'   => $this->toEmail,
                'to_user_id' => $this->userId,
                'subject'    => $this->subject,
                'scene'      => $this->scene,
                'batch_id'   => $this->batchId,
            ]);
        } catch (\Throwable $e) {
            Log::error('SendMailJob failed', [
                'to'     => $this->toEmail,
                'error'  => $e->getMessage(),
            ]);

            MailLog::create([
                'type'       => $this->type,
                'status'     => MailLog::STATUS_FAILED,
                'to_email'   => $this->toEmail,
                'to_user_id' => $this->userId,
                'subject'    => $this->subject,
                'error'      => $e->getMessage(),
                'scene'      => $this->scene,
                'batch_id'   => $this->batchId,
            ]);
        }
    }
}