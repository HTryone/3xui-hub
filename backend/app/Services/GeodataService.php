<?php

namespace App\Services;

use App\Models\Node;
use App\Models\NodeApiCommand;
use Illuminate\Support\Facades\Http;

/**
 * Geo 数据（geoip.dat / geosite.dat）管理：面板下载 + 分发到节点（issue #40）。
 *
 * 链路与 agent 自升级同构（节点只连面板，不碰 GitHub）：
 *   面板从白名单源下载 → storage/app/xray/geo/ 落盘（If-Modified-Since 条件请求）
 *   → 入队 geodata_sync 指令（payload 带 sha256 + 面板下载相对路径）
 *   → agent 从面板下载 → sha256 校验 → 原子替换 NODE_DIR 下的 .dat
 *   → 有文件实际变化才 restart xray（sha 相同则跳过，不打扰存量节点）。
 *
 * 失败安全：下载/校验任何一步失败都保留旧文件；节点侧同样先校验后替换。
 */
class GeodataService
{
    /** 白名单：仅这两个文件可下载/分发（对齐上游 3x-ui geofileAllowlist 的口径）。 */
    public const FILES = [
        'geoip.dat' => 'https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat',
        'geosite.dat' => 'https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat',
    ];

    /** 单文件大小下限（防把错误页 / 空内容当成功下载）。真实的 .dat 都是 MB 级。 */
    private const MIN_BYTES = 100 * 1024;

    public function dir(): string
    {
        $configured = trim((string) config('panel.geodata_dir', ''), "/\\");

        return $configured !== '' ? $configured : storage_path('app/xray/geo');
    }

    public function filePath(string $name): string
    {
        return $this->dir() . '/' . $name;
    }

    public function manifestPath(): string
    {
        return $this->dir() . '/manifest.json';
    }

    /** 本地 manifest（下载记录：size / sha256 / updated_at / checked_at）。 */
    public function manifest(): array
    {
        $path = $this->manifestPath();
        if (! is_file($path)) {
            return [];
        }
        $data = json_decode((string) file_get_contents($path), true);

        return is_array($data) ? $data : [];
    }

    /** 面板当前文件状态（管理页展示）。 */
    public function status(): array
    {
        $manifest = $this->manifest();
        $files = [];

        foreach (array_keys(self::FILES) as $name) {
            $path = $this->filePath($name);
            $meta = is_array($manifest[$name] ?? null) ? $manifest[$name] : [];
            $exists = is_file($path);

            $files[] = [
                'name' => $name,
                'exists' => $exists,
                'size' => $exists ? (int) filesize($path) : 0,
                // 实时计算，不依赖历史记录（面板文件被手工动过也能如实反映）
                'sha256' => $exists ? (string) hash_file('sha256', $path) : '',
                'updated_at' => $meta['updated_at'] ?? ($exists ? date('c', (int) filemtime($path)) : null),
                'checked_at' => $meta['checked_at'] ?? null,
            ];
        }

        return $files;
    }

    /**
     * 从白名单源检查并更新（304 或内容相同 → unchanged，不算更新）。
     *
     * @return array<string, array{status:string, size?:int, sha256?:string, message?:string}>
     */
    public function update(): array
    {
        if (! is_dir($this->dir()) && ! mkdir($this->dir(), 0755, true) && ! is_dir($this->dir())) {
            return array_map(
                fn () => ['status' => 'failed', 'message' => '创建目录失败: ' . $this->dir()],
                self::FILES
            );
        }

        $results = [];
        foreach (self::FILES as $name => $url) {
            $results[$name] = $this->updateOne($name, $url);
        }

        return $results;
    }

    private function updateOne(string $name, string $url): array
    {
        $target = $this->filePath($name);
        $tmp = $target . '.tmp';

        $headers = ['User-Agent' => 'ControlHub-Geodata/1.0'];
        if (is_file($target)) {
            // 条件请求：没变化就 304 跳过（对齐上游 3x-ui 的做法）
            $headers['If-Modified-Since'] = gmdate('D, d M Y H:i:s', (int) filemtime($target)) . ' GMT';
        }

        try {
            $response = Http::timeout(300)->withHeaders($headers)->get($url);
        } catch (\Throwable $e) {
            $this->touchChecked($name);

            return ['status' => 'failed', 'message' => '下载失败: ' . $e->getMessage()];
        }

        if ($response->status() === 304) {
            $this->touchChecked($name);

            return [
                'status' => 'unchanged',
                'size' => is_file($target) ? (int) filesize($target) : 0,
                'sha256' => is_file($target) ? (string) hash_file('sha256', $target) : '',
            ];
        }

        if (! $response->successful()) {
            return ['status' => 'failed', 'message' => '上游返回 HTTP ' . $response->status()];
        }

        $body = $response->body();
        if (strlen($body) < self::MIN_BYTES) {
            return ['status' => 'failed', 'message' => '内容过小（' . strlen($body) . ' 字节），疑似错误响应，已放弃'];
        }

        $sha = hash('sha256', $body);

        // 磁盘上已是同一份 → 不重写（保持 mtime，维持条件请求语义）
        if (is_file($target) && hash_file('sha256', $target) === $sha) {
            $this->touchChecked($name);

            return ['status' => 'unchanged', 'size' => strlen($body), 'sha256' => $sha];
        }

        // 先落临时文件、再原子替换：任何一步失败旧文件原样保留
        if (@file_put_contents($tmp, $body) === false) {
            return ['status' => 'failed', 'message' => '写入临时文件失败'];
        }
        if (! @rename($tmp, $target)) {
            @unlink($tmp);

            return ['status' => 'failed', 'message' => '替换文件失败'];
        }

        $this->recordUpdated($name, strlen($body), $sha);

        return ['status' => 'updated', 'size' => strlen($body), 'sha256' => $sha];
    }

    /**
     * 把面板当前的 geo 文件分发到所有 enabled 的 xray 节点。
     * 幂等：节点已有未终态的 geodata_sync 指令 → 跳过（等结果，防指令风暴）。
     *
     * @return array{queued:int, skipped:int, missing:bool}
     */
    public function dispatchToNodes(): array
    {
        $files = [];
        foreach (array_keys(self::FILES) as $name) {
            $path = $this->filePath($name);
            if (! is_file($path)) {
                continue;
            }
            $files[] = [
                'name' => $name,
                'size' => (int) filesize($path),
                'sha256' => (string) hash_file('sha256', $path),
                'url' => '/node-bin/geo/' . $name,
            ];
        }

        if ($files === []) {
            return ['queued' => 0, 'skipped' => 0, 'missing' => true];
        }

        $queued = 0;
        $skipped = 0;

        foreach (Node::where('enabled', true)->where('driver_type', 'xray')->orderBy('id')->get() as $node) {
            $inFlight = NodeApiCommand::where('node_id', $node->id)
                ->where('action', NodeApiCommand::ACTION_GEODATA_SYNC)
                ->whereIn('status', [NodeApiCommand::STATUS_PENDING, NodeApiCommand::STATUS_ACKNOWLEDGED])
                ->exists();

            if ($inFlight) {
                $skipped++;
                continue;
            }

            NodeApiCommand::create([
                'node_id' => $node->id,
                'user_id' => null,
                'action' => NodeApiCommand::ACTION_GEODATA_SYNC,
                'payload' => ['files' => $files],
                'status' => NodeApiCommand::STATUS_PENDING,
            ]);
            $queued++;
        }

        return ['queued' => $queued, 'skipped' => $skipped, 'missing' => false];
    }

    /** 各节点最近一次 geodata 同步状态（管理页展示）。 */
    public function nodeStatuses(): array
    {
        return Node::where('driver_type', 'xray')
            ->orderBy('id')
            ->get()
            ->map(function (Node $node) {
                $cmd = NodeApiCommand::where('node_id', $node->id)
                    ->where('action', NodeApiCommand::ACTION_GEODATA_SYNC)
                    ->orderByDesc('id')
                    ->first();

                return [
                    'node_id' => $node->id,
                    'node_name' => $node->name,
                    'enabled' => (bool) $node->enabled,
                    'status' => $cmd?->status,
                    'result' => $cmd?->result,
                    'finished_at' => $cmd?->updated_at?->toIso8601String(),
                ];
            })
            ->values()
            ->all();
    }

    private function recordUpdated(string $name, int $size, string $sha): void
    {
        $manifest = $this->manifest();
        $manifest[$name] = [
            'size' => $size,
            'sha256' => $sha,
            'updated_at' => now()->toIso8601String(),
            'checked_at' => now()->toIso8601String(),
        ];
        $this->writeManifest($manifest);
    }

    private function touchChecked(string $name): void
    {
        $manifest = $this->manifest();
        $manifest[$name] = array_merge(
            is_array($manifest[$name] ?? null) ? $manifest[$name] : [],
            ['checked_at' => now()->toIso8601String()]
        );
        $this->writeManifest($manifest);
    }

    private function writeManifest(array $manifest): void
    {
        @file_put_contents(
            $this->manifestPath(),
            json_encode($manifest, JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES)
        );
    }
}
