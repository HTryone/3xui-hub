<?php

namespace App\Services;

use App\Models\MonitorDaily;
use App\Models\MonitorSample;
use App\Models\Node;
use Carbon\CarbonImmutable;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Log;

/**
 * 机器流量监控：采样落库 + 差分口径 + 只读查询（summary / series）。
 *
 * 贯穿的一条原则（同 NodeMetricsTest 头注释）：**没有数据就是 null，不能兜底成 0**。
 * 采样侧「取不到值就不写行」，查询侧「没样本的字段是 null」—— 否则「节点没上报」
 * 会被前端画成「流量 0」，采购决策直接看错数。
 *
 * 差分口径只有一处定义（deltaBetween / sumDeltas）：
 *   每条样本的 delta = max(0, cum - 前一条 cum)（负差钳 0 —— 机器重启 / 计数器
 *   回绕 / 网卡重连都会让累计值倒退，倒退段绝不能被当成「负流量」把总量吃掉）。
 *   「前一条」允许落在窗口或分组之外，取不到时按 0 起算（不把首次采样的累计值
 *   当成一段流量）。
 */
class MonitorService
{
    /** 采样节流窗口（秒）：距最近一条样本不足此时长就跳过，不写重复行。 */
    private const THROTTLE_SECONDS = 240;

    /**
     * 记录一次机器流量采样（挂健康检查，异常绝不影响健康检查主流程）。
     *
     * @param  ?array  $health  3x-ui healthCheck() 的返回数组（含 netSent/netRecv/cpu/mem）
     * @return bool 是否真的写入了采样行
     */
    public function record(Node $node, ?array $health): bool
    {
        // 全兜：采样是附加信息，任何异常（列不存在 / 库抖动 / driver_config 结构异常）
        // 都只 log 一次，绝不向外抛 —— 否则会把每 5 分钟的健康检查整轮打断。
        try {
            return $this->doRecord($node, $health);
        } catch (\Throwable $e) {
            Log::warning('机器流量采样失败，已跳过', [
                'node_id' => $node->id,
                'error' => $e->getMessage(),
            ]);

            return false;
        }
    }

    private function doRecord(Node $node, ?array $health): bool
    {
        [$sent, $recv, $cpu, $mem] = $this->extract($node, $health);

        // 拿不到累计值（null / 缺字段）→ 不写行（绝不写 0：0 会被下游当成
        // 「累计归零」，差分时凭空造出一段流量）。
        if ($sent === null || $recv === null) {
            return false;
        }

        // 节流：最近一条样本不足 THROTTLE_SECONDS → 跳过。
        // 正常的 5 分钟轮不受影响；队列失败重试（retry_after=180s）会被挡住，不写重复行。
        $last = MonitorSample::where('node_id', $node->id)->max('sampled_at');
        if ($last !== null && CarbonImmutable::parse($last)->diffInSeconds(now()) < self::THROTTLE_SECONDS) {
            return false;
        }

        MonitorSample::create([
            'node_id' => $node->id,
            'sent' => $sent,
            'recv' => $recv,
            'cpu' => $cpu,
            'mem' => $mem,
            'sampled_at' => now(),
        ]);

        return true;
    }

    /**
     * 从节点本身与健康检查结果里取 (sent, recv, cpu, mem)，取不到一律 null。
     *
     * - 3x-ui：累计值来自 healthCheck() 透传的 netSent/netRecv；cpu/mem 同源。
     * - xray：累计值来自 driver_config.agent 的 net_total_up/down；cpu/mem 与它们
     *   同源（agent 的同一份快照）。**跟随读取**的意义：老 agent 只有 net_total 没有
     *   cpu/mem（或反之）时，不会出现「网卡值来自这一刻、cpu 来自上一次上报」的半新半旧。
     *
     * @return array{0: ?int, 1: ?int, 2: ?float, 3: ?float}
     */
    private function extract(Node $node, ?array $health): array
    {
        if ($node->isXray()) {
            $agent = $node->driver_config['agent'] ?? null;

            if (! is_array($agent)) {
                return [null, null, null, null];
            }

            // agent 侧 net_total 缺失（老 agent / 非 Linux 读不到 /proc/net/dev）→ null。
            // 数值合法性检查与 cpu/mem 一致：只认非负数值。
            $sent = $this->nonNegativeInt($agent['net_total_up'] ?? null);
            $recv = $this->nonNegativeInt($agent['net_total_down'] ?? null);

            if ($sent === null || $recv === null) {
                return [null, null, null, null];
            }

            return [
                $sent,
                $recv,
                $this->numericOrNull($agent['cpu'] ?? null),
                $this->numericOrNull($agent['mem'] ?? null),
            ];
        }

        $sent = $this->nonNegativeInt($health['netSent'] ?? null);
        $recv = $this->nonNegativeInt($health['netRecv'] ?? null);

        if ($sent === null || $recv === null) {
            return [null, null, null, null];
        }

        return [
            $sent,
            $recv,
            $this->numericOrNull($health['cpu'] ?? null),
            $this->numericOrNull($health['mem'] ?? null),
        ];
    }

    private function nonNegativeInt(mixed $value): ?int
    {
        if (! is_numeric($value)) {
            return null;
        }

        $int = (int) $value;

        return $int < 0 ? null : $int;
    }

    private function numericOrNull(mixed $value): ?float
    {
        return is_numeric($value) ? (float) $value : null;
    }

    // ===== 差分口径（唯一一处）=====

    /**
     * 单条样本相对前一条累计值的增量，负差钳 0。
     * $prev 为 null（首次采样 / 取不到「前一条」）时整段按 0 起算。
     *
     * @param  array{sent:int|string,recv:int|string}  $cur
     * @param  ?array{sent:int|string,recv:int|string}  $prev
     * @return array{0:int,1:int} [sentDelta, recvDelta]
     */
    public static function deltas(array $cur, ?array $prev): array
    {
        if ($prev === null) {
            return [0, 0];
        }

        return [
            max(0, (int) $cur['sent'] - (int) $prev['sent']),
            max(0, (int) $cur['recv'] - (int) $prev['recv']),
        ];
    }

    // ===== 只读查询（admin-api）=====

    /**
     * 窗口定义 → [CarbonImmutable $start, CarbonImmutable $end, string $key]。
     * 非法 window 值一律回退 24h（不报错 —— 前端传错参数不该把监控页打挂）。
     *
     * 站点时区与全站一致：日界（month 的 1 日 00:00）按 config('panel.business_timezone')
     * 取，再换算成 app.timezone 下的同一时刻参与比较（同 StatsController 的口径）。
     */
    public function window(?string $window): array
    {
        $key = in_array($window, ['24h', '7d', '30d', 'month'], true) ? $window : '24h';
        $end = CarbonImmutable::now();

        $start = match ($key) {
            '7d' => $end->subDays(7),
            '30d' => $end->subDays(30),
            'month' => $this->monthStart(),
            default => $end->subHours(24),
        };

        return [$start, $end, $key];
    }

    /** 本月 1 日 00:00（站点时区），换算成 app.timezone 下的同一时刻。 */
    private function monthStart(): CarbonImmutable
    {
        $appTimezone = (string) config('app.timezone');
        $businessTimezone = (string) (config('panel.business_timezone') ?: $appTimezone);

        return CarbonImmutable::now($businessTimezone)->startOfMonth()->setTimezone($appTimezone);
    }

    /**
     * 窗口内每台启用节点的合计与 cpu/mem 极值。
     *
     * 口径：合计 up/down = 窗口内每条样本的增量之和（窗口首条的「前一条」允许取
     * 窗口外最近一条；取不到按 0）。cpu_avg/cpu_max/mem_avg/mem_max 为该节点的
     * 采样值聚合，无样本时为 null。
     *
     * @return array{totals: array{up:int,down:int,total:int}, window: array{key:string,start:string,end:string}, nodes: list<array>}
     */
    public function summary(?string $window): array
    {
        [$start, $end, $key] = $this->window($window);

        $nodes = Node::where('enabled', true)->orderBy('id')->get();
        $rows = [];

        foreach ($nodes as $node) {
            [$up, $down] = $this->windowDeltas((int) $node->id, $start, $end);

            // 该窗口内没有任何样本 → 流量与 cpu/mem 全 null（没有数据就是 null，不是 0）
            if ($up === null && $down === null) {
                $rows[] = [
                    'id' => $node->id,
                    'name' => $node->name,
                    'up' => null,
                    'down' => null,
                    'total' => null,
                    'cpu_avg' => null,
                    'cpu_max' => null,
                    'mem_avg' => null,
                    'mem_max' => null,
                ];
                continue;
            }

            $stats = MonitorSample::where('node_id', $node->id)
                ->whereBetween('sampled_at', [$start, $end])
                ->selectRaw('AVG(cpu) as cpu_avg, MAX(cpu) as cpu_max, AVG(mem) as mem_avg, MAX(mem) as mem_max')
                ->first();

            $rows[] = [
                'id' => $node->id,
                'name' => $node->name,
                'up' => $up ?? 0,
                'down' => $down ?? 0,
                'total' => ($up ?? 0) + ($down ?? 0),
                'cpu_avg' => $stats?->cpu_avg === null ? null : round((float) $stats->cpu_avg, 2),
                'cpu_max' => $stats?->cpu_max === null ? null : round((float) $stats->cpu_max, 2),
                'mem_avg' => $stats?->mem_avg === null ? null : round((float) $stats->mem_avg, 2),
                'mem_max' => $stats?->mem_max === null ? null : round((float) $stats->mem_max, 2),
            ];
        }

        // 合计只累加有数据的节点（没数据的节点是 null，不是 0，加进去也不改变结果，
        // 但显式跳过更能表达「无数据不参与合计」的意图）。
        $up = 0;
        $down = 0;
        foreach ($rows as $row) {
            if ($row['up'] === null) {
                continue;
            }
            $up += $row['up'];
            $down += $row['down'];
        }

        return [
            'totals' => ['up' => $up, 'down' => $down, 'total' => $up + $down],
            'window' => [
                'key' => $key,
                'start' => $start->toIso8601String(),
                'end' => $end->toIso8601String(),
            ],
            'nodes' => $rows,
        ];
    }

    /**
     * 窗口内每台节点的增量序列。
     * 粒度：24h→原始 5 分钟点；7d→按小时 sum；30d/month→按天 sum。
     *
     * @return list<array{node_id:int,name:string,points:list<array{t:int,up:int,down:int}>}>
     */
    public function series(?string $window): array
    {
        [$start, $end, $key] = $this->window($window);

        $bucket = match ($key) {
            '7d' => 'hour',
            '30d', 'month' => 'day',
            default => 'raw',
        };

        $out = [];
        foreach (Node::where('enabled', true)->orderBy('id')->get() as $node) {
            $out[] = [
                'node_id' => $node->id,
                'name' => $node->name,
                'points' => $this->points((int) $node->id, $start, $end, $bucket),
            ];
        }

        return $out;
    }

    /**
     * 某节点在窗口内的 [up, down] 增量合计。
     * 同窗口内一条样本都没有 → [null, null]（与 sumDeltas 的「空集合」区分：
     * 空集合代表「有采样但都是首条」，null 代表「压根没采样」）。
     *
     * @return array{0: ?int, 1: ?int}
     */
    private function windowDeltas(int $nodeId, CarbonImmutable $start, CarbonImmutable $end): array
    {
        $points = $this->points($nodeId, $start, $end, 'raw');

        if ($points === []) {
            return [null, null];
        }

        $up = 0;
        $down = 0;
        foreach ($points as $p) {
            $up += $p['up'];
            $down += $p['down'];
        }

        return [$up, $down];
    }

    /**
     * 生成某节点的增量点列。
     *
     * @param  'raw'|'hour'|'day'  $bucket
     * @return list<array{t:int,up:int,down:int}>
     */
    private function points(int $nodeId, CarbonImmutable $start, CarbonImmutable $end, string $bucket): array
    {
        // 「窗口首条的前一条」：窗口外最近一条（取不到就不受影响，按 0 起算）。
        $prev = MonitorSample::where('node_id', $nodeId)
            ->where('sampled_at', '<', $start)
            ->orderByDesc('sampled_at')
            ->first();

        $rows = MonitorSample::where('node_id', $nodeId)
            ->whereBetween('sampled_at', [$start, $end])
            ->orderBy('sampled_at')
            ->get();

        if ($rows->isEmpty()) {
            return [];
        }

        // 逐条差分（口径与 rollup 完全一致），再按粒度归并
        $buckets = []; // bucketKey => [t, up, down]
        foreach ($rows as $row) {
            [$dSent, $dRecv] = self::deltas(
                ['sent' => $row->sent, 'recv' => $row->recv],
                $prev === null ? null : ['sent' => $prev->sent, 'recv' => $prev->recv],
            );
            $prev = $row;

            $t = $this->bucketStart(CarbonImmutable::parse($row->sampled_at), $bucket);
            $bucketKey = $t->getTimestamp();

            if (! isset($buckets[$bucketKey])) {
                $buckets[$bucketKey] = ['t' => $bucketKey, 'up' => 0, 'down' => 0];
            }
            $buckets[$bucketKey]['up'] += $dSent;
            $buckets[$bucketKey]['down'] += $dRecv;
        }

        ksort($buckets);

        return array_values($buckets);
    }

    /** 采样时刻归并到的粒度起点（站点时区取日界，与 month 窗口口径一致）。 */
    private function bucketStart(CarbonImmutable $at, string $bucket): CarbonImmutable
    {
        $appTimezone = (string) config('app.timezone');

        return match ($bucket) {
            'hour' => $at->setTimezone($appTimezone)->startOfHour(),
            'day' => $at->setTimezone($this->businessTimezone())->startOfDay(),
            default => $at,
        };
    }

    private function businessTimezone(): string
    {
        return (string) (config('panel.business_timezone') ?: config('app.timezone'));
    }

    // ===== 降采样（monitor:rollup 用）=====

    /**
     * 把过期的 L0 样本折成 monitor_daily 的当天增量并删除原始样本。
     *
     * 幂等保证（重复运行结果一致）：
     *   1. upsert 是**覆盖**不是累加 —— 同一个 (node_id, date) 重算多少遍结果都一样；
     *   2. 只处理「本次仍有过期样本」的天，没有过期样本的天不动 daily 现值；
     *   3. 聚合与删除在同一个事务里 —— 算完落库再删样本，中途失败整段回滚，
     *      不存在「样本删了但 daily 没写」这种永久丢数据的窗口。
     *
     * @return array{samples:int, groups:int, nodes:int}
     */
    public function rollup(?CarbonImmutable $now = null): array
    {
        $retainDays = max(1, (int) config('monitor.retain_days', 90));
        $cutoff = ($now ?? CarbonImmutable::now())->subDays($retainDays);

        return DB::transaction(function () use ($cutoff) {
            $expired = MonitorSample::where('sampled_at', '<', $cutoff)
                ->orderBy('node_id')
                ->orderBy('sampled_at')
                ->get();

            if ($expired->isEmpty()) {
                return ['samples' => 0, 'groups' => 0, 'nodes' => 0];
            }

            $timezone = $this->businessTimezone();
            $groups = [];   // "nodeId|date" => collected aggregate
            $lastSeen = []; // nodeId => 上一条已处理的样本

            // 已按 (node_id, sampled_at) 升序取回：逐条与前一条差分即可。
            // 注意「前一条」天然覆盖跨天边界 —— 某个自然日的首条样本，它的前一条
            // 就是前一天的最后一条；跨天那一段字节因此计入新的一天（与 events 口径一致）。
            // 而该节点最早的那条过期样本前一条必为空（任何更早的样本也都过期、
            // 也在这批里且排在它前面），按 0 起算，正是「取不到按 0」的口径。
            foreach ($expired as $sample) {
                $nodeId = (int) $sample->node_id;
                $date = CarbonImmutable::parse($sample->sampled_at)->setTimezone($timezone)->toDateString();

                $prev = $lastSeen[$nodeId] ?? null;
                $lastSeen[$nodeId] = $sample;

                [$dSent, $dRecv] = self::deltas(
                    ['sent' => $sample->sent, 'recv' => $sample->recv],
                    $prev === null ? null : ['sent' => $prev->sent, 'recv' => $prev->recv],
                );

                $groupKey = $nodeId . '|' . $date;

                if (! isset($groups[$groupKey])) {
                    $groups[$groupKey] = [
                        'node_id' => $nodeId,
                        'date' => $date,
                        'sent_delta' => 0,
                        'recv_delta' => 0,
                    ];
                }

                $groups[$groupKey]['sent_delta'] += $dSent;
                $groups[$groupKey]['recv_delta'] += $dRecv;
            }

            // 覆盖写：同 (node_id, date) 重跑结果一致（幂等的核心）
            foreach ($groups as $g) {
                MonitorDaily::updateOrCreate(
                    ['node_id' => $g['node_id'], 'date' => $g['date']],
                    ['sent_delta' => $g['sent_delta'], 'recv_delta' => $g['recv_delta']],
                );
            }

            $deleted = MonitorSample::where('sampled_at', '<', $cutoff)->delete();

            return [
                'samples' => $deleted,
                'groups' => count($groups),
                'nodes' => count(array_unique(array_column($groups, 'node_id'))),
            ];
        });
    }
}
