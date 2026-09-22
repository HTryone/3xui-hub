<?php

namespace App\Jobs;

use App\Models\Domain;
use App\Services\AsyncTaskService;
use App\Services\DomainService;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;
use Illuminate\Support\Facades\Log;

/**
 * 域名一键应用（AsyncTask item）。
 *
 * 接收 domain id + 动作（apply|renew|remove），调 DomainService 走 3hub-domain 助手。
 * DomainService 对「助手未安装 / 签发失败」只回写 ssl_status=failed，不抛异常，
 * 因此本 Job 在本地开发环境也会「成功」执行完（队列不重试、item 标 succeeded）。
 */
class ApplyDomainJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public int $tries = 3;
    public array $backoff = [30, 60, 120];

    public function __construct(
        public int $domainId,
        public string $action, // apply|renew|remove|status
        public ?int $taskId = null,
        public ?string $itemKey = null,
        public ?string $domainName = null,
    ) {}

    public function handle(
        DomainService $service,
        AsyncTaskService $tasks,
    ): void {
        if ($this->taskId !== null && $this->itemKey !== null) {
            if (!$tasks->claimItem($this->taskId, $this->itemKey)) {
                return; // 已被处理（幂等重放）
            }
        }

        $domain = Domain::find($this->domainId);
        if ($domain === null) {
            // remove 场景：行已删，用派发时携带的域名跑助手（按剩余 enabled 域名重建 conf）
            if ($this->domainName === null) {
                if ($this->taskId !== null && $this->itemKey !== null) {
                    $tasks->completeItem($this->taskId, $this->itemKey);
                }
                return;
            }
            $domain = new Domain(['domain' => $this->domainName]);
        }

        match ($this->action) {
            'apply' => $service->apply($domain),
            'renew' => $service->renew($domain),
            'remove' => $service->remove($domain),
            'status' => $service->status($domain),
            default => throw new \InvalidArgumentException("不支持的域名动作：{$this->action}"),
        };

        if ($this->taskId !== null && $this->itemKey !== null) {
            $tasks->completeItem($this->taskId, $this->itemKey);
        }
    }

    public function failed(\Throwable $exception): void
    {
        if ($this->taskId === null || $this->itemKey === null) {
            return;
        }
        Log::error('域名任务执行失败', [
            'domain_id' => $this->domainId,
            'domain' => $this->domainName,
            'action' => $this->action,
            'error' => $exception->getMessage(),
        ]);

        app(AsyncTaskService::class)->failItem(
            $this->taskId,
            $this->itemKey,
            '域名操作失败：'.$exception->getMessage()
        );
    }
}
