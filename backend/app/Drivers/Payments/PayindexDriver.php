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
 * payindex 协议驱动 —— 现有生产网关协议，从 PaymentService 原样搬入，行为零变化。
 *
 * 签名：过滤空值 → ksort → http_build_query → 拼 `&key={api_key}` → MD5 → 大写。
 * 下单响应：status==1 且 h5_url 非空 → 支付链接；否则 msg 为拒绝原因。
 * 回调：memberid/orderid/amount/transaction_id/datetime/returncode 验签，returncode=='00'。
 */
class PayindexDriver extends AbstractPaymentDriver implements PaymentDriverInterface
{
    public function name(): string
    {
        return 'payindex';
    }

    public function supportsQuery(): bool
    {
        return true;
    }

    /**
     * payindex MD5 签名（与旧 PaymentService::generateSign 逐字节一致）。
     */
    public static function signParams(array $params, string $apiKey): string
    {
        $filtered = array_filter($params, fn ($v) => $v !== '' && $v !== null);
        ksort($filtered);
        $stringSignTemp = http_build_query($filtered) . '&key=' . $apiKey;

        return strtoupper(md5($stringSignTemp));
    }

    /**
     * 下单，返回 [h5_url, 网关拒绝原因] 语义的 PaymentResult：
     * - 成功：PaymentResult::ok(h5_url)
     * - 网关明确拒绝：PaymentResult::fail(msg)   ← 调用方据此作废旧单重建
     * - 网络/解析异常：error=null                ← 属于临时故障，调用方不得作废订单
     */
    public function pay(Order $order, PaymentConfig $config): PaymentResult
    {
        $params = [
            'pay_memberid' => $this->cfg($config, 'member_id'),
            'pay_orderid' => $order->order_no,
            // 复用旧单重发时用「当前时间」，不能用 $order->created_at：老单可能是几天前创建的，
            // 网关会因时间差过大拒单或对账错乱（现场有 7 月老单被翻出重发的案例）。
            'pay_applydate' => now()->format('Y-m-d H:i:s'),
            'pay_bankcode' => $this->cfg($config, 'bank_code'),
            'pay_notifyurl' => $this->notifyUrl($config),
            'pay_callbackurl' => $this->returnUrl(),
            'pay_amount' => $this->money((float) $order->amount),
            'pay_productname' => '套餐购买-' . ($order->plan->name ?? ''),
            'pay_ip' => request()->ip() ?: ($order->pay_ip ?: '127.0.0.1'),
            'pay_type' => 'JSON',
        ];

        $params['pay_md5sign'] = self::signParams($params, (string) $this->cfg($config, 'api_key', ''));

        try {
            $response = Http::asForm()
                ->timeout(10)
                ->post($this->cfg($config, 'gateway', ''), $params);

            $data = $response->json();

            Log::info('支付网关响应', ['order_no' => $order->order_no, 'response' => $data]);

            if (($data['status'] ?? 0) == 1 && !empty($data['h5_url'])) {
                return PaymentResult::ok($data['h5_url']);
            }

            $msg = (string) ($data['msg'] ?? 'unknown');
            Log::error('支付网关下单失败', ['order_no' => $order->order_no, 'msg' => $msg]);

            // 网关明确拒绝：把原因带出去，调用方可据此决定是否作废旧单重建
            return PaymentResult::fail($msg);
        } catch (\Throwable $e) {
            Log::error('支付网关请求异常', ['order_no' => $order->order_no, 'error' => $e->getMessage()]);

            // 网络异常：原因留空（null），调用方不得作废订单（用户可能已在支付）
            return new PaymentResult(success: false, error: null);
        }
    }

    /**
     * 回调验签 + 状态判断（金额比对由 PaymentService 统一做）。
     */
    public function handleCallback(array $data, PaymentConfig $config): CallbackResult
    {
        $memberId = $data['memberid'] ?? '';
        $orderId = $data['orderid'] ?? '';
        $amount = $data['amount'] ?? '';
        $returnCode = $data['returncode'] ?? '';
        $sign = $data['sign'] ?? '';
        $tradeNo = $data['transaction_id'] ?? '';

        if ($this->cfg($config, 'member_id') != $memberId) {
            Log::error('支付回调: 商户号不匹配', ['order_no' => $orderId]);

            return CallbackResult::fail('商户号不匹配');
        }

        if (!$this->verifySignature($data, $config)) {
            Log::error('支付回调: 签名验证失败', ['order_no' => $orderId]);

            return CallbackResult::fail('签名验证失败');
        }

        if ($returnCode !== '00') {
            Log::error('支付回调: 状态异常', ['returncode' => $returnCode]);

            return CallbackResult::fail('状态异常');
        }

        return CallbackResult::paid((string) $orderId, (string) $tradeNo, (float) $amount);
    }

    public function verifySignature(array $data, PaymentConfig $config): bool
    {
        $verifyData = [
            'memberid' => $data['memberid'] ?? '',
            'orderid' => $data['orderid'] ?? '',
            'amount' => $data['amount'] ?? '',
            'transaction_id' => $data['transaction_id'] ?? '',
            'datetime' => $data['datetime'] ?? '',
            'returncode' => $data['returncode'] ?? '',
        ];
        $expectedSign = self::signParams($verifyData, (string) $this->cfg($config, 'api_key', ''));

        return strcasecmp((string) ($data['sign'] ?? ''), $expectedSign) === 0;
    }

    /**
     * 查单：POST query_gateway，returncode=='00' 且 trade_state=='SUCCESS' 视为已支付。
     */
    public function query(Order $order, PaymentConfig $config): ?array
    {
        $queryGateway = $this->cfg($config, 'query_gateway');
        if (!$queryGateway) {
            return null;
        }

        $params = [
            'pay_memberid' => $this->cfg($config, 'member_id'),
            'pay_orderid' => $order->order_no,
        ];
        $params['pay_md5sign'] = self::signParams($params, (string) $this->cfg($config, 'api_key', ''));

        try {
            $response = Http::asForm()
                ->timeout(10)
                ->post($queryGateway, $params)
                ->json();

            if (($response['returncode'] ?? '') === '00') {
                $tradeState = $response['trade_state'] ?? '';

                return [
                    'status' => $tradeState === 'SUCCESS' ? 'paid' : 'pending',
                    'trade_state' => $tradeState,
                    'trade_no' => $response['transaction_id'] ?? null,
                ];
            }
        } catch (\Throwable $e) {
            Log::error('订单查询失败', ['order_no' => $order->order_no, 'error' => $e->getMessage()]);
        }

        return null;
    }

    /** payindex 网关要求应答 OK / FAIL */
    public function notifyResponse(bool $success): string
    {
        return $success ? 'OK' : 'FAIL';
    }

    public static function definition(): array
    {
        return [
            'key' => 'payindex',
            'label' => 'PayIndex（现网关协议）',
            'description' => '当前生产在用协议：MD5 签名（ksort + http_build_query + &key= 密钥，大写）。支持查单。',
            'supports_query' => true,
            'fields' => [
                [
                    'name' => 'gateway',
                    'label' => '支付网关地址',
                    'placeholder' => 'https://xxx.com/Pay_Index.html',
                    'required' => true,
                    'secret' => false,
                ],
                [
                    'name' => 'query_gateway',
                    'label' => '查询网关地址',
                    'placeholder' => 'https://xxx.com/Pay_Trade_query.html',
                    'required' => false,
                    'secret' => false,
                ],
                [
                    'name' => 'member_id',
                    'label' => '商户号',
                    'placeholder' => '输入商户号',
                    'required' => true,
                    'secret' => false,
                ],
                [
                    'name' => 'api_key',
                    'label' => 'API密钥',
                    'placeholder' => '输入API密钥',
                    'required' => true,
                    'secret' => true,
                ],
                [
                    'name' => 'bank_code',
                    'label' => '银行编码（支付平台后台查看）',
                    'placeholder' => '如：1=支付宝, 2=微信',
                    'required' => true,
                    'secret' => false,
                ],
            ],
        ];
    }
}
