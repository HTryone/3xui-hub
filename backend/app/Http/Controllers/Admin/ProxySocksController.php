<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\ProxySocks;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;

/**
 * 手工 socks 货源管理（/admin-api/proxy-store/socks）。
 *
 * 站长手上有一些 socks（IP:端口 + 账号密码），没有 API，只能手工录入；
 * 一条记录可以挂到一个「共享型 SKU」上，卖给多个用户共用同一套凭据。
 *
 * 支持两种批量粘贴格式（一行一条，空行/# 注释忽略）：
 *   1) host:port:username:password[:country]
 *   2) socks5://username:password@host:port
 *
 * 密码加密存储，列表/详情接口一律不回（只回 has_password）。
 */
class ProxySocksController extends Controller
{
    use ApiResponse;

    /** GET /admin-api/proxy-store/socks */
    public function index(): JsonResponse
    {
        $items = ProxySocks::query()->orderByDesc('id')->get();

        return $this->success([
            'items' => $items->map(fn (ProxySocks $s) => $this->present($s))->values(),
        ]);
    }

    /** POST /admin-api/proxy-store/socks/import  body: { text: string } */
    public function import(Request $request): JsonResponse
    {
        $data = $request->validate([
            'text' => ['required', 'string', 'max:20000'],
        ]);

        $created = 0;
        $skipped = 0;
        $invalid = 0;
        $errors = [];

        foreach (preg_split('/\r\n|\r|\n/', $data['text']) as $index => $raw) {
            $line = trim($raw);
            if ($line === '' || str_starts_with($line, '#')) {
                continue;
            }

            $parsed = $this->parseLine($line);
            if ($parsed === null) {
                $invalid++;
                if (count($errors) < 5) {
                    $errors[] = '第 ' . ($index + 1) . " 行无法识别：{$line}";
                }
                continue;
            }

            // 去重：同 host + port + username 视为同一条
            $exists = ProxySocks::query()
                ->where('host', $parsed['host'])
                ->where('port', $parsed['port'])
                ->where(function ($q) use ($parsed) {
                    if ($parsed['username'] === null) {
                        $q->whereNull('username')->orWhere('username', '');
                    } else {
                        $q->where('username', $parsed['username']);
                    }
                })
                ->exists();

            if ($exists) {
                $skipped++;
                continue;
            }

            ProxySocks::create([
                'name' => $parsed['host'] . ':' . $parsed['port'],
                'host' => $parsed['host'],
                'port' => $parsed['port'],
                'username' => $parsed['username'],
                'password' => $parsed['password'],
                'country' => $parsed['country'],
                'is_active' => true,
            ]);
            $created++;
        }

        // 只有半截失败也照实回传，管理员能据此修格式重贴
        return $this->success([
            'created' => $created,
            'skipped' => $skipped,
            'invalid' => $invalid,
            'errors' => $errors,
        ], "导入完成：新增 {$created}，重复跳过 {$skipped}" . ($invalid ? "，无法识别 {$invalid}" : ''));
    }

    /** PUT /admin-api/proxy-store/socks/{sock} */
    public function update(Request $request, ProxySocks $sock): JsonResponse
    {
        $data = $request->validate([
            'name' => ['sometimes', 'string', 'max:100'],
            'host' => ['sometimes', 'string', 'max:190'],
            'port' => ['sometimes', 'integer', 'min:1', 'max:65535'],
            'username' => ['sometimes', 'nullable', 'string', 'max:190'],
            'password' => ['sometimes', 'nullable', 'string', 'max:190'],
            'country' => ['sometimes', 'nullable', 'string', 'max:16'],
            'city' => ['sometimes', 'nullable', 'string', 'max:64'],
            'note' => ['sometimes', 'nullable', 'string', 'max:255'],
            'is_active' => ['sometimes', 'boolean'],
        ]);

        // 密码留空 = 不修改
        if (array_key_exists('password', $data) && ($data['password'] === null || $data['password'] === '')) {
            unset($data['password']);
        }

        $sock->fill($data)->save();

        return $this->success($this->present($sock->fresh()), '已保存');
    }

    /** DELETE /admin-api/proxy-store/socks/{sock} */
    public function destroy(ProxySocks $sock): JsonResponse
    {
        // 被 SKU 挂着的 socks 不能直接删（那个 SKU 已卖出/在售，删了用户凭据就成孤儿）
        $used = \App\Models\ProxySku::where('source_type', 'socks')->where('source_id', $sock->id)->exists();
        if ($used) {
            return $this->error('这条 socks 已被某个代理规格使用，请先把规格下线或换货源', 400);
        }

        $sock->delete();

        return $this->success(null, '已删除');
    }

    // ===== 内部 =====

    /** 解析一行，识别不了返回 null */
    private function parseLine(string $line): ?array
    {
        // 形式 1：socks5://user:pass@host:port
        if (preg_match('#^(?:socks5h?|http)://(?:([^:@/]+):([^@/]*)@)?([^:/]+):(\d{1,5})$#i', $line, $m)) {
            return [
                'host' => $m[3],
                'port' => (int) $m[4],
                'username' => ($m[1] ?? '') !== '' ? $m[1] : null,
                'password' => ($m[2] ?? '') !== '' ? $m[2] : null,
                'country' => null,
            ];
        }

        // 形式 2：host:port:user:pass[:country]
        $parts = explode(':', $line);
        if (count($parts) >= 2 && is_numeric($parts[1])) {
            $port = (int) $parts[1];
            if ($port < 1 || $port > 65535 || $parts[0] === '') {
                return null;
            }

            return [
                'host' => $parts[0],
                'port' => $port,
                'username' => ($parts[2] ?? '') !== '' ? $parts[2] : null,
                'password' => ($parts[3] ?? '') !== '' ? $parts[3] : null,
                'country' => isset($parts[4]) && $parts[4] !== '' ? strtoupper($parts[4]) : null,
            ];
        }

        return null;
    }

    private function present(ProxySocks $sock): array
    {
        return [
            'id' => $sock->id,
            'name' => $sock->name,
            'host' => $sock->host,
            'port' => (int) $sock->port,
            'username' => $sock->username,
            // 只回「有没有密码」，明文/密文都不出接口
            'has_password' => (string) $sock->password !== '',
            'country' => $sock->country,
            'city' => $sock->city,
            'note' => $sock->note,
            'is_active' => (bool) $sock->is_active,
            'created_at' => $sock->created_at?->toDateTimeString(),
        ];
    }
}
