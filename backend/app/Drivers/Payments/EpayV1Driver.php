<?php

namespace App\Drivers\Payments;

use App\Drivers\Contracts\CallbackResult;
use App\Drivers\Contracts\PaymentDriverInterface;
use App\Drivers\Contracts\PaymentResult;
use App\Models\Order;
use App\Models\PaymentConfig;
use Illuminate\Support\Facades\Http;
use Illuminate\Support\Facades\Log;

/**
 * 易支付（标准版）协议驱动。
 *
 * 下单：POST {gateway}/submit.php，参数 pid/type/out_trade_no/notify_url/return_url/name/money/sign/sign_type=MD5
 * 签名：排除 sign、sign_type 与空值 → 参数名 ASCII 升序 k=v&k=v → 末尾直接追加密钥本身 → MD5 小写
 * 响应：JSON {code:1,url:"..."} 取 url；code!=1 取 msg 为拒绝原因
 * 回调：pid/trade_no/out_trade_no/type/name/money/trade_status/sign/sign_type（GET）
 * 判定：验签 + pid==member_id + trade_status==TRADE_SUCCESS + 金额误差 ≤ 0.01
 * 查单：多数 epay 站点无查单接口 → query() 返回 null
 */
class EpayV1Driver extends AbstractPaymentDriver implements PaymentDriverInterface
{
    public function name(): string
    {
        return 'epay_v1';
    }

    public function supportsQuery(): bool
    {
        return false;
    }

    /**
     * 易支付 MD5 签名：排除 sign/sign_type 与空值 → ASCII 升序拼接 → 末尾直接追加密钥 → 小写 MD5。
     */
    public static function signParams(array $params, string $key): string
    {
        unset($params['sign'], $params['sign_type']);
        $params = array_filter($params, fn ($v) => $v !== '' && $v !== null);
        ksort($params);

        $pairs = [];
        foreach ($params as $k => $v) {
            $pairs[] = $k . '=' . $v;
        }

        return md5(implode('&', $pairs) . $key);
    }

    /**
     * 下单。网关地址通常以 /submit.php 结尾，配置没带就自动补。
     * 返回语义同 payindex：fail(msg)=网关拒绝，error=null=网络/解析异常。
     */
    public function pay(Order $order, PaymentConfig $config): PaymentResult
    {
        $gateway = (string) $this->cfg($config, 'gateway', '');
        if (!str_ends_with($gateway, '/submit.php')) {
            $gateway = rtrim($gateway, '/') . '/submit.php';
        }

        $params = [
            'pid' => $this->cfg($config, 'member_id'),
            'type' => $this->cfg($config, 'bank_code'),
            'out_trade_no' => $order->order_no,
            'notify_url' => $this->notifyUrl($config),
            'return_url' => $this->returnUrl(),
            'name' => '套餐购买-' . ($order->plan->name ?? ''),
            'money' => $this->money((float) $order->amount),
        ];
        $params['sign'] = self::signParams($params, (string) $this->cfg($config, 'api_key', ''));
        $params['sign_type'] = 'MD5';

        try {
            $response = Http::asForm()
                ->timeout(10)
                ->post($gateway, $params);

            $data = $response->json();

            Log::info('易支付网关响应', ['order_no' => $order->order_no, 'response' => $data]);

            if (!is_array($data)) {
                // 非 JSON 响应按解析异常处理（临时故障，不得作废订单）
                Log::error('易支付网关响应非 JSON', ['order_no' => $order->order_no]);

                return new PaymentResult(success: false, error: null);
            }

            if ((int) ($data['code'] ?? 0) === 1 && !empty($data['url'])) {
                return PaymentResult::ok((string) $data['url']);
            }

            $msg = (string) ($data['msg'] ?? 'unknown');
            Log::error('易支付下单失败', ['order_no' => $order->order_no, 'msg' => $msg]);

            return PaymentResult::fail($msg);
        } catch (\Throwable $e) {
            Log::error('易支付网关请求异常', ['order_no' => $order->order_no, 'error' => $e->getMessage()]);

            return new PaymentResult(success: false, error: null);
        }
    }

    /**
     * 回调验签 + 状态判断（金额比对由 PaymentService 统一做）。
     */
    public function handleCallback(array $data, PaymentConfig $config): CallbackResult
    {
        $orderId = (string) ($data['out_trade_no'] ?? '');
        $pid = (string) ($data['pid'] ?? '');

        if ($this->cfg($config, 'member_id') != $pid) {
            Log::error('易支付回调: 商户号不匹配', ['order_no' => $orderId, 'pid' => $pid]);

            return CallbackResult::fail('商户号不匹配');
        }

        if (!$this->verifySignature($data, $config)) {
            Log::error('易支付回调: 签名验证失败', ['order_no' => $orderId]);

            return CallbackResult::fail('签名验证失败');
        }

        if (($data['trade_status'] ?? '') !== 'TRADE_SUCCESS') {
            Log::error('易支付回调: 状态异常', ['trade_status' => $data['trade_status'] ?? '']);

            return CallbackResult::fail('状态异常');
        }

        return CallbackResult::paid(
            $orderId,
            (string) ($data['trade_no'] ?? ''),
            (float) ($data['money'] ?? 0),
        );
    }

    public function verifySignature(array $data, PaymentConfig $config): bool
    {
        $sign = (string) ($data['sign'] ?? '');
        if ($sign === '') {
            return false;
        }

        $expected = self::signParams($data, (string) $this->cfg($config, 'api_key', ''));

        return strcasecmp($sign, $expected) === 0;
    }

    /** 该协议不支持查单 */
    public function query(Order $order, PaymentConfig $config): ?array
    {
        return null;
    }

    /** 易支付要求应答 success（或 ok）/ fail */
    public function notifyResponse(bool $success): string
    {
        return $success ? 'success' : 'fail';
    }

    public static function definition(): array
    {
        return [
            'key' => 'epay_v1',
            'label' => '易支付（标准版）',
            'description' => '标准易支付：MD5 签名（排除 sign/sign_type，末尾直接追加密钥，小写）。该协议不支持查单。',
            'supports_query' => false,
            'fields' => [
                [
                    'name' => 'gateway',
                    'label' => '支付网关地址',
                    'placeholder' => 'https://xxx.com/（结尾可不带 submit.php，自动补）',
                    'required' => true,
                    'secret' => false,
                ],
                [
                    'name' => 'member_id',
                    'label' => '商户ID（pid）',
                    'placeholder' => '输入商户ID',
                    'required' => true,
                    'secret' => false,
                ],
                [
                    'name' => 'api_key',
                    'label' => '通信密钥（key）',
                    'placeholder' => '输入通信密钥',
                    'required' => true,
                    'secret' => true,
                ],
                [
                    'name' => 'bank_code',
                    'label' => '支付方式（type）',
                    'placeholder' => '如：alipay / wxpay / qqpay',
                    'required' => true,
                    'secret' => false,
                ],
            ],
        ];
    }
}
