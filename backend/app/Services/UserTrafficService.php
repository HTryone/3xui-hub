<?php

namespace App\Services;

use App\Models\Node;
use Illuminate\Support\Facades\DB;

/**
 * 用户×节点 流量的只读查询（admin-api 用）。
 *
 * 窗口 → 表：24h 查小时表（滑动窗口），7d/30d 查天表。
 * 三个口径：
 *   - weighted：乘节点倍率后的字节，与 users.traffic_used 同源（默认排序、计费口径）；
 *   - up / down：面板侧原始字节（未乘倍率），用于行为分析、与机器网卡口径对账；
 *   - nic_*：机器网卡口径（MonitorService::summary，仅启用节点），与用户腿并排对照。
 * 没有数据的用户/节点不出现在结果里（缺数据 ≠ 0，同 MonitorService 的既有原则）。
 */
class UserTrafficService
{
    private const DEFAULT_LIMIT = 50;
    private const MAX_LIMIT = 500;

    public function __construct(private MonitorService $monitor)
    {
    }

    private function businessTimezone(): string
    {
        return (string) (config('panel.business_timezone') ?: config('app.timezone'));
    }

    /**
     * 窗口 → [表名, 桶列, 起, 止, 窗口键]；非法值回退 24h。
     *
     * 24h 走小时表：起点是「24 小时前的整点」，含当前未走完的小时（滑动窗口）。
     * 7d/30d 走天表：业务时区自然日 [today-6, today] / [today-29, today]。
     */
    private function resolve(?string $window): array
    {
        $tz = $this->businessTimezone();
        $today = now()->setTimezone($tz)->toDateString();

        return match ($window) {
            '7d' => ['user_traffic_daily', 'date', now()->setTimezone($tz)->startOfDay()->subDays(6)->toDateString(), $today, '7d'],
            '30d' => ['user_traffic_daily', 'date', now()->setTimezone($tz)->startOfDay()->subDays(29)->toDateString(), $today, '30d'],
            default => ['user_traffic_hourly', 'hour_start',
                now()->subDay()->startOfHour()->toDateTimeString(),
                now()->toDateTimeString(), '24h'],
        };
    }

    private function limit(int $limit): int
    {
        return max(1, min(self::MAX_LIMIT, $limit ?: self::DEFAULT_LIMIT));
    }

    /** 数据起始日缓存（请求级：service 每个请求重新实例化，不会跨请求串数据）。 */
    private ?string $dataStart = null;
    private bool $dataStartResolved = false;

    /**
     * 数据起始日（业务时区 Y-m-d）：表里最早那个桶，没有数据时 null。
     *
     * 新表从上线当天开始累积，7d/30d 窗口要等满 —— 界面必须能标出「数据自哪天起」，
     * 否则用户会把「还没攒够」读成「这几天没人用」。min() 走桶列前缀索引，代价可忽略。
     */
    private function dataStart(string $table, string $col): ?string
    {
        if ($this->dataStartResolved) {
            return $this->dataStart;
        }

        $this->dataStartResolved = true;

        $earliest = DB::table($table)->min($col);
        if ($earliest === null) {
            return $this->dataStart = null;
        }

        return $this->dataStart = $col === 'date'
            ? (string) $earliest
            : \Illuminate\Support\Carbon::parse($earliest, config('app.timezone'))
                ->setTimezone($this->businessTimezone())
                ->toDateString();
    }

    /** 按用户排行（窗口内合计，加权降序）。 */
    public function userRanking(?string $window, int $limit = self::DEFAULT_LIMIT): array
    {
        [$table, $col, $start, $end, $key] = $this->resolve($window);

        $rows = DB::table("{$table} as t")
            ->join('users as u', 'u.id', '=', 't.user_id')
            ->leftJoin('plans as p', 'p.id', '=', 'u.plan_id')
            ->whereBetween("t.{$col}", [$start, $end])
            ->groupBy('t.user_id', 'u.email', 'p.type', 'p.name', 'p.months')
            ->orderByDesc('weighted')
            ->limit($this->limit($limit))
            ->select('t.user_id', 'u.email', 'p.type as plan_type', 'p.name as plan_name', 'p.months as plan_months')
            ->selectRaw('SUM(t.weighted_delta) as weighted, SUM(t.up_delta) as up, SUM(t.down_delta) as down')
            ->get()
            ->map(fn ($r) => [
                'user_id' => (int) $r->user_id,
                'email' => $r->email,
                // period（周期套餐）/ total（总量套餐）/ null（无套餐，理论上同步侧已跳过）
                'plan_type' => $r->plan_type,
                'plan_name' => $r->plan_name,
                'plan_months' => $r->plan_months === null ? null : (int) $r->plan_months,
                'weighted' => (int) $r->weighted,
                'up' => (int) $r->up,
                'down' => (int) $r->down,
            ])
            ->all();

        return ['window' => $key, 'start' => $start, 'end' => $end,
                'data_start' => $this->dataStart($table, $col), 'rows' => $rows];
    }

    /**
     * 按机器排行：用户腿（原始/加权）与网卡口径并排。
     *
     * 两套数字本来就不同源，这里**不换算、不对齐**，只是并排给出：
     *   - up / down / weighted：user_traffic_* 里的用户代理流量（每个字节只计一次，用户腿）；
     *   - nic_up / nic_down / nic_total：monitor_* 网卡口径（同一字节一进一出走两遍，
     *     还含 SSH / 系统更新 / 探针等非代理流量），来源 MonitorService::summary()。
     * 网卡口径只覆盖排序并集里的 enabled 节点；某台机器在该窗口没有任何采样时
     * nic_* 是 null（不是 0）。multiplier 走 TrafficSyncService::multiplierFor()（唯一入口）。
     */
    public function nodeRanking(?string $window): array
    {
        [$table, $col, $start, $end, $key] = $this->resolve($window);

        $rows = DB::table("{$table} as t")
            ->join('nodes as n', 'n.id', '=', 't.node_id')
            ->whereBetween("t.{$col}", [$start, $end])
            ->groupBy('t.node_id', 'n.name')
            ->select('t.node_id', 'n.name')
            ->selectRaw('SUM(t.weighted_delta) as weighted, SUM(t.up_delta) as up, SUM(t.down_delta) as down')
            ->get()
            ->map(fn ($r) => [
                'node_id' => (int) $r->node_id,
                'name' => $r->name,
                'weighted' => (int) $r->weighted,
                'up' => (int) $r->up,
                'down' => (int) $r->down,
            ])
            ->keyBy('node_id')
            ->all();

        // 机器卡的排序主键是网卡口径（设计稿：网卡合计一列在最前），所以列表以
        // 「窗口内有用户流量的节点 ∪ 网卡口径里的启用节点」为准 —— 只有系统流量、
        // 没有用户代理流量的机器也要出现，否则「机器流量」会被读成「用户流量按机器汇总」。
        $rows = $this->attachMachineColumns($rows, $window);

        uasort($rows, function (array $a, array $b) {
            // 网卡合计降序（null 垫底）→ 用户原始降序（null 垫底）→ 名称，保证稳定
            foreach (['nic_total', 'user_raw'] as $field) {
                $left = $a[$field];
                $right = $b[$field];
                if ($left === $right) {
                    continue;
                }
                if ($left === null) {
                    return 1;
                }
                if ($right === null) {
                    return -1;
                }

                return $right <=> $left;
            }

            return strcmp((string) $a['name'], (string) $b['name']);
        });

        return ['window' => $key, 'start' => $start, 'end' => $end,
                'data_start' => $this->dataStart($table, $col),
                'rows' => array_values($rows)];
    }

    /**
     * 单用户按机器明细（核心视角：这个用户在哪台机器用了多少）。
     *
     * 每行额外带：
     *   - multiplier：该机器当前的节点倍率（TrafficSyncService::multiplierFor，唯一入口）；
     *   - share：该机器占**本用户窗口内加权合计**的百分比（0–100，保留 1 位）。
     * 明细展开图要画「哪台机器占了几成」，占比必须以用户自己为分母 ——
     * 用榜单分母会得出「64% + 22% + 14% = 100% 但都不是这个人」的错觉。
     */
    public function userNodeBreakdown(int $userId, ?string $window): array
    {
        [$table, $col, $start, $end, $key] = $this->resolve($window);

        $rows = DB::table("{$table} as t")
            ->join('nodes as n', 'n.id', '=', 't.node_id')
            ->where('t.user_id', $userId)
            ->whereBetween("t.{$col}", [$start, $end])
            ->groupBy('t.node_id', 'n.name')
            ->orderByDesc('weighted')
            ->select('t.node_id', 'n.name')
            ->selectRaw('SUM(t.weighted_delta) as weighted, SUM(t.up_delta) as up, SUM(t.down_delta) as down')
            ->get()
            ->map(fn ($r) => [
                'node_id' => (int) $r->node_id,
                'name' => $r->name,
                'weighted' => (int) $r->weighted,
                'up' => (int) $r->up,
                'down' => (int) $r->down,
            ])
            ->all();

        $nodes = $this->nodesByIds(array_column($rows, 'node_id'));
        $total = array_sum(array_column($rows, 'weighted'));

        $rows = array_map(function (array $row) use ($nodes, $total) {
            $node = $nodes[$row['node_id']] ?? null;
            $row['multiplier'] = $node === null ? null : TrafficSyncService::multiplierFor($node);
            // 合计为 0（窗口内只有方向增量、计费增量为 0）→ 占比无意义，给 null 而不是 0%
            $row['share'] = $total > 0
                ? round($row['weighted'] / $total * 100, 1)
                : null;

            return $row;
        }, $rows);

        return ['window' => $key, 'start' => $start, 'end' => $end,
                'data_start' => $this->dataStart($table, $col), 'rows' => $rows];
    }

    /**
     * 机器排行左侧的「网卡口径」三列 + 倍率（设计稿：两列并排对照）。
     *
     * 网卡数据现取 MonitorService::summary()（仅 enabled 节点、窗口内无采样则 null），
     * 不重造一套采样查询。summary 的窗口语义与这里略有出入（7d/30d 是「now-7d」的
     * 滑动窗口，本服务是业务时区自然日），边界那一小段本来就不可能逐字节对齐 ——
     * 两套口径只做量级对照，界面上的说明也是这么写的。
     *
     * @param  array<int, array>  $rows  以 node_id 为键（缺该节点的用户腿时补零/null）
     * @return array<int, array>
     */
    private function attachMachineColumns(array $rows, ?string $window): array
    {
        $summary = $this->monitor->summary($window);

        $nic = [];
        foreach (($summary['nodes'] ?? []) as $item) {
            $nic[(int) $item['id']] = $item;
        }

        // 并集：用户腿里出现过的节点 + 网卡口径里的全部启用节点
        $ids = array_values(array_unique(array_merge(array_keys($rows), array_keys($nic))));
        $nodes = $this->nodesByIds($ids);

        $out = [];
        foreach ($ids as $id) {
            $id = (int) $id;
            $node = $nodes[$id] ?? null;
            $item = $nic[$id] ?? null;
            $row = $rows[$id] ?? null;

            // 只有系统流量、没有用户代理流量的机器：用户腿三列是 null（不是 0）——
            // 「这窗口里没有这个用户的流量」和「用户流量确实是 0」在库里不可区分时，
            // 按「没有数据」处理，与 MonitorService 的既有原则一致。
            $out[$id] = [
                'node_id' => $id,
                'name' => $row['name'] ?? ($node?->name ?? $item['name'] ?? "#{$id}"),
                'weighted' => $row['weighted'] ?? null,
                'up' => $row['up'] ?? null,
                'down' => $row['down'] ?? null,
                // 用户代理的**原始**字节（上下行合计，未乘倍率）——与网卡列并排对照用。
                // 与 weighted 同表不同列：weighted 是计费口径，这里每一字节只计一次。
                'user_raw' => $row === null ? null : $row['up'] + $row['down'],
                'multiplier' => $node === null ? null : TrafficSyncService::multiplierFor($node),
                'nic_up' => $item['up'] ?? null,
                'nic_down' => $item['down'] ?? null,
                'nic_total' => $item['total'] ?? null,
            ];
        }

        return $out;
    }

    /**
     * id → Node（含软删/停用也要能算出倍率，所以不加 enabled 过滤）。
     * 倍率是节点配置属性，与「窗口内有没有流量」无关。
     *
     * @param  list<int|string>  $ids
     * @return array<int, Node>
     */
    private function nodesByIds(array $ids): array
    {
        if ($ids === []) {
            return [];
        }

        return Node::whereIn('id', $ids)->get()->keyBy('id')->all();
    }
}
