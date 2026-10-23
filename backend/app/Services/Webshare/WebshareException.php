<?php

namespace App\Services\Webshare;

/**
 * Webshare 上游调用异常。
 *
 * 触发场景：
 *  - HTTP 请求本身失败（连接超时、DNS、5xx 等，Guzzle 异常被包装）
 *  - 上游返回 429（限流）—— 单独带 $retryAfter，调用方可据此退避
 *  - 上游返回 403 —— 站长没开通子用户 API 权限，这是产品前置条件，提示要不同
 *  - 响应体不是合法 JSON，或业务失败（detail 字段带原因）
 *
 * 不做重试：限流退避与批次节奏由调用方（ProxyProviderService / 定时同步）决定，
 * 客户端只负责把「为什么失败」讲清楚。
 */
class WebshareException extends \RuntimeException
{
    public function __construct(
        string $message,
        private readonly ?int $status = null,
        private readonly ?int $retryAfter = null,
    ) {
        parent::__construct($message);
    }

    /** 上游 HTTP 状态码；网络层失败（未拿到响应）时为 null。 */
    public function status(): ?int
    {
        return $this->status;
    }

    /** 429 时上游给的 Retry-After 秒数；没有则为 null。 */
    public function retryAfter(): ?int
    {
        return $this->retryAfter;
    }

    public function isRateLimited(): bool
    {
        return $this->status === 429;
    }

    /** 403 = 站长没在 Webshare 侧开通子用户 API 权限。 */
    public function isSubuserForbidden(): bool
    {
        return $this->status === 403;
    }
}
