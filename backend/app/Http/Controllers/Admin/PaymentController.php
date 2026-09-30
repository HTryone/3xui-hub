<?php

namespace App\Http\Controllers\Admin;

use App\Drivers\Contracts\PaymentDriverInterface;
use App\Drivers\DriverRegistry;
use App\Http\Controllers\Controller;
use App\Models\PaymentConfig;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;
use Illuminate\Validation\ValidationException;

/**
 * Admin 支付配置管理。
 * GET /admin-api/payments/protocols 协议清单（前端动态渲染表单用）
 * GET/POST/PUT/DELETE /admin-api/payments
 */
class PaymentController extends Controller
{
    use ApiResponse;

    /** 可回落到旧列的协议字段（与 payment_configs 列名一致，保老数据/老接口语义） */
    private const LEGACY_COLUMNS = ['gateway', 'query_gateway', 'member_id', 'api_key', 'bank_code'];

    public function index(): \Illuminate\Http\JsonResponse
    {
        $configs = PaymentConfig::orderByDesc('id')->get();

        return $this->success($configs->map(fn (PaymentConfig $c) => $this->present($c))->values());
    }

    /**
     * 协议清单：后台动态表单元数据（key/label/说明/fields）。
     * 从 DriverRegistry 里已注册的支付驱动取 definition()，新增协议自动出现在清单里。
     */
    public function protocols(DriverRegistry $registry): \Illuminate\Http\JsonResponse
    {
        $list = [];

        foreach ($registry->names() as $name) {
            $driver = $registry->get($name);
            if (!$driver instanceof PaymentDriverInterface) {
                continue;
            }

            $def = $driver::definition();

            $list[] = [
                'key' => $def['key'],
                'label' => $def['label'],
                'description' => $def['description'] ?? '',
                'supports_query' => (bool) ($def['supports_query'] ?? false),
                'fields' => array_map(fn (array $f) => [
                    'name' => $f['name'],
                    'label' => $f['label'],
                    'placeholder' => $f['placeholder'] ?? '',
                    'required' => (bool) ($f['required'] ?? false),
                    'isSecret' => (bool) ($f['secret'] ?? false),
                ], $def['fields'] ?? []),
            ];
        }

        return $this->success($list);
    }

    public function show(PaymentConfig $payment): \Illuminate\Http\JsonResponse
    {
        return $this->success($this->present($payment, true));
    }

    public function store(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $this->validateConfig($request);

        $config = PaymentConfig::create($data);

        return $this->success($this->present($config), '创建成功');
    }

    public function update(Request $request, PaymentConfig $payment): \Illuminate\Http\JsonResponse
    {
        $data = $this->validateConfig($request, true, $payment);

        $payment->forceFill($data)->save();

        return $this->success($this->present($payment), '更新成功');
    }

    public function destroy(PaymentConfig $payment): \Illuminate\Http\JsonResponse
    {
        $payment->delete();

        return $this->success(null, '已删除');
    }

    /**
     * 校验并归一化配置：
     * - 协议特有字段全部收进 driver_config（JSON），同时把与旧列同名的字段写回旧列（兼容老读取方）
     * - 密文字段「留空不修改」：空值从 driver_config 剔除后与旧值合并，旧列同步不动
     * - 老客户端仍可直接传 gateway/member_id/api_key 等顶层字段，会并入 driver_config
     */
    private function validateConfig(Request $request, bool $isUpdate = false, ?PaymentConfig $existing = null): array
    {
        $data = $request->validate([
            'name' => ['required', 'string', 'max:64'],
            'driver_type' => ['sometimes', 'nullable', 'string', 'max:32'],
            'driver_config' => ['sometimes', 'nullable', 'array'],
            'notify_url' => ['sometimes', 'nullable', 'string', 'max:255'],
            'enabled' => ['sometimes', 'boolean'],
            // 兼容老客户端顶层字段（并入 driver_config）
            'gateway' => ['sometimes', 'nullable', 'string', 'max:255'],
            'query_gateway' => ['sometimes', 'nullable', 'string', 'max:255'],
            'member_id' => ['sometimes', 'nullable', 'string', 'max:64'],
            'api_key' => ['sometimes', 'nullable', 'string', 'max:255'],
            'bank_code' => ['sometimes', 'nullable', 'string', 'max:64'],
        ]);

        $driverType = $this->normalizeDriverType($data['driver_type'] ?? null);

        $definition = $this->definitionFor($driverType);
        if ($definition === null) {
            // 兜底：normalizeDriverType 已按 registry 判定过，正常走不到这里
            throw ValidationException::withMessages(['driver_type' => '未知支付协议']);
        }

        $fields = collect($definition['fields'])->keyBy('name');

        // 老客户端顶层字段并入 driver_config（driver_config 优先）
        $incoming = is_array($data['driver_config'] ?? null) ? $data['driver_config'] : [];
        foreach (self::LEGACY_COLUMNS as $col) {
            if (!array_key_exists($col, $incoming) && array_key_exists($col, $data)) {
                $incoming[$col] = $data[$col];
            }
        }

        // 只保留该协议定义过的字段（防脏写）
        $incoming = array_intersect_key($incoming, $fields->all());

        $config = [];
        foreach ($fields as $name => $field) {
            if (!array_key_exists($name, $incoming)) {
                continue;
            }
            $value = is_scalar($incoming[$name]) ? trim((string) $incoming[$name]) : '';

            if ($value === '' && !empty($field['secret']) && $isUpdate) {
                // 密文字段留空 = 不修改
                continue;
            }

            $config[$name] = $value;
        }

        // 必填校验：更新时密文字段已有旧值可豁免（留空不修改）
        foreach ($fields as $name => $field) {
            if (empty($field['required']) || ($config[$name] ?? '') !== '') {
                continue;
            }

            $hasOldSecret = !empty($field['secret']) && $isUpdate && $existing !== null
                && (($existing->driver_config[$name] ?? null) || ($name === 'api_key' && $existing->api_key));
            if ($hasOldSecret) {
                continue;
            }

            throw ValidationException::withMessages([
                'driver_config.' . $name => $field['label'] . '为必填项',
            ]);
        }

        // 更新：密文空值已被剔除，这里与旧 driver_config 合并，保证「留空不修改」
        if ($isUpdate && $existing !== null) {
            $config = array_merge($existing->driver_config ?? [], $config);
        }

        $out = [
            'name' => $data['name'],
            'driver_type' => $driverType,
            'driver_config' => $config,
        ];
        // notify_url 同 enabled：更新时不传就整个键不进 $out，forceFill 自然不碰该列。
        // 改动前规则是 'sometimes'，第三方脚本 PUT 只带 name 时若被无条件写 null，
        // 老用户的回调地址会被清空 → 网关回调直接收不到。
        if (array_key_exists('notify_url', $data)) {
            $out['notify_url'] = $data['notify_url'];
        } elseif (!$isUpdate) {
            $out['notify_url'] = null;
        }
        if (array_key_exists('enabled', $data)) {
            $out['enabled'] = $data['enabled'];
        } elseif (!$isUpdate) {
            $out['enabled'] = true;
        }

        // 与旧列同名的字段写回旧列（methods 列表、老代码读列不受影响）；
        // 新建时 NOT NULL 列补空串；更新时密文空值路径不在 $config 里 → 列不动
        foreach (self::LEGACY_COLUMNS as $col) {
            if (array_key_exists($col, $config)) {
                $out[$col] = $config[$col];
            } elseif (!$isUpdate) {
                $out[$col] = ($col === 'query_gateway') ? null : '';
            }
        }

        return $out;
    }

    private function definitionFor(string $driverType): ?array
    {
        $registry = app(DriverRegistry::class);

        foreach ($registry->names() as $name) {
            $driver = $registry->get($name);
            if (!$driver instanceof PaymentDriverInterface) {
                continue;
            }
            $def = $driver::definition();
            if (($def['key'] ?? '') === $driverType) {
                return $def;
            }
        }

        return null;
    }

    /**
     * 归一化 driver_type：为空、或不在 registry 已注册支付驱动里 → 一律回退 'payindex'。
     *
     * 按 registry 实际注册情况判定，不硬编码具体协议名 —— 否则以后每加一个协议，
     * 存量数据里的旧值（如建表列默认值 'wwspay'）都会重新撞上同一个坑：
     * 前端把库里读到的旧值原样提交回来，validateConfig 判定为「未知支付协议」→ 422，
     * 老用户进后台连改个配置名称都保存不了。
     *
     * 与 PaymentService::driverFor() 的回退口径一致：库里存的是什么不重要，
     * 出站一律按已注册的协议解释。
     */
    private function normalizeDriverType(?string $type): string
    {
        $type = trim((string) $type);
        if ($type !== '' && $this->definitionFor($type) !== null) {
            return $type;
        }

        return 'payindex';
    }

    private function present(PaymentConfig $c, bool $full = false): array
    {
        $driverType = $this->normalizeDriverType($c->driver_type);

        $data = [
            'id' => $c->id,
            'name' => $c->name,
            'driver_type' => $driverType,
            'gateway' => $c->gateway,
            'query_gateway' => $c->query_gateway,
            'member_id' => $c->member_id,
            'notify_url' => $c->notify_url,
            'bank_code' => $c->bank_code,
            'enabled' => (bool) $c->enabled,
            'created_at' => $c->created_at?->toIso8601String(),
        ];

        if ($full) {
            $data['api_key'] = $c->api_key;
        } else {
            $data['has_api_key'] = !empty($c->api_key) || !empty($c->driver_config['api_key']);
        }

        // 协议字段（driver_config）回显：密文脱敏；老数据 driver_config 为空 → 用旧列拼视图
        $data['driver_config'] = $this->presentDriverConfig($c, $driverType);

        return $data;
    }

    /**
     * 密文字段值不回显（置空），前端「留空不修改」。
     * 老数据 driver_config 为空 → 用旧列拼一份视图，编辑回显不丢字段。
     */
    private function presentDriverConfig(PaymentConfig $c, string $driverType): array
    {
        $config = is_array($c->driver_config) ? $c->driver_config : [];

        if (!$config) {
            $config = array_filter([
                'gateway' => $c->gateway,
                'query_gateway' => $c->query_gateway,
                'member_id' => $c->member_id,
                'api_key' => $c->api_key,
                'bank_code' => $c->bank_code,
            ], fn ($v) => $v !== null && $v !== '');
        }

        $definition = $this->definitionFor($driverType);
        if ($definition) {
            foreach ($definition['fields'] as $field) {
                if (!empty($field['secret']) && array_key_exists($field['name'], $config)) {
                    $config[$field['name']] = '';
                }
            }
        } else {
            // 未知协议也绝不回显 api_key
            unset($config['api_key']);
        }

        return $config;
    }
}
