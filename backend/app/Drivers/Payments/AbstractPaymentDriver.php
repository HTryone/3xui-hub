<?php

namespace App\Drivers\Payments;

use App\Drivers\Capability;
use App\Drivers\Contracts\CallbackResult;
use App\Drivers\Contracts\PaymentResult;
use App\Models\Domain;
use App\Models\Order;
use App\Models\PaymentConfig;

/**
 * 支付驱动公共基类 —— 取值、回调地址、能力声明等协议无关逻辑。
 *
 * 取值顺序：driver_config（新，JSON）→ 旧列（老数据，driver_config 为空时零影响）→ 默认值。
 * 协议特有字段全部存 driver_config，老列只服务 payindex 及历史数据。
 */
abstract class AbstractPaymentDriver
{
    /** 可回落到旧列的字段名（与 payment_configs 表列名一致） */
    private const LEGACY_COLUMNS = ['gateway', 'query_gateway', 'member_id', 'api_key', 'bank_code'];

    /** 读取配置项：driver_config 优先，空则回退旧列 */
    protected function cfg(PaymentConfig $config, string $key, ?string $default = null): ?string
    {
        $dc = $config->driver_config ?? [];
        if (is_array($dc) && array_key_exists($key, $dc) && $dc[$key] !== null && $dc[$key] !== '') {
            return (string) $dc[$key];
        }

        if (in_array($key, self::LEGACY_COLUMNS, true)) {
            $v = $config->getAttribute($key);
            if ($v !== null && $v !== '') {
                return (string) $v;
            }
        }

        return $default;
    }

    /** 异步通知地址：配置里带完整 URL 用配置，否则用主域名默认值（网关固定打主域） */
    protected function notifyUrl(PaymentConfig $config): string
    {
        $u = $config->notify_url;

        return ($u && str_starts_with($u, 'http'))
            ? $u
            : $this->defaultNotifyUrl();
    }

    /** 缺省异步通知地址：domains 主域名 + /api/payment/notify；无主域行（老站）回退 url() */
    protected function defaultNotifyUrl(): string
    {
        $primary = Domain::where('is_primary', true)->where('enabled', true)->first();
        if (!$primary) {
            return url('/api/payment/notify');
        }

        // scheme https 优先（域名接入层默认走 TLS）
        return 'https://' . $primary->domain . '/api/payment/notify';
    }

    /** 同步回跳地址 = 下单请求所在域名（多域名：b.zes.one 下单回 b.zes.one，不回主域） */
    protected function returnUrl(): string
    {
        return request()->getSchemeAndHttpHost() . '/';
    }

    /** 金额统一两位小数 */
    protected function money(float $amount): string
    {
        return number_format($amount, 2, '.', '');
    }

    // ── DriverInterface ──

    public function version(): string
    {
        return '1.0';
    }

    public function supports(string $capability): bool
    {
        return in_array(Capability::tryFrom($capability), $this->capabilityList());
    }

    public function capabilities(): array
    {
        return array_map(fn (Capability $c) => $c->value, $this->capabilityList());
    }

    /** @return Capability[] */
    protected function capabilityList(): array
    {
        $caps = [Capability::PAYMENT_PAY, Capability::PAYMENT_CALLBACK];
        if ($this->supportsQuery()) {
            $caps[] = Capability::PAYMENT_QUERY;
        }

        return $caps;
    }

    /** 是否支持查单（多数 epay 站点无查单接口） */
    public function supportsQuery(): bool
    {
        return false;
    }

    // ── PaymentDriverInterface 默认实现 ──

    /** 退款：默认不支持 */
    public function refund(Order $order, PaymentConfig $config, ?float $amount = null): bool
    {
        return false;
    }

    /** 回调成功/失败应答文本（各网关要求不同） */
    public function notifyResponse(bool $success): string
    {
        return $success ? 'success' : 'fail';
    }

    // ── 子类必须实现 ──

    abstract public function name(): string;

    abstract public function pay(Order $order, PaymentConfig $config): PaymentResult;

    abstract public function verifySignature(array $data, PaymentConfig $config): bool;

    abstract public function handleCallback(array $data, PaymentConfig $config): CallbackResult;

    /** @return array{status: string, trade_state: string, trade_no?: ?string}|null */
    abstract public function query(Order $order, PaymentConfig $config): ?array;

    /** 协议定义（后台动态表单用） */
    abstract public static function definition(): array;
}
