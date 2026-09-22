<?php

return [
    /*
     | 未进入终态的异步任务在该时间（分钟）后被定时清扫标记为失败，
     | 防止任务因 Worker 被杀 / Job 丢失 / 锁竞争等原因永久停在 running。
     */
    'stale_after_minutes' => (int) env('TASKS_STALE_AFTER_MINUTES', 10),

    /*
     | 节点锁竞争时，同步 Job 最多重新入队次数；超过后标记任务失败。
     | 每次重新入队延迟 30 秒，5 次约 150 秒，可覆盖 120 秒的节点锁 TTL。
     */
    'max_lock_releases' => (int) env('TASKS_MAX_LOCK_RELEASES', 5),

    /*
     | 入站同步一个 Job 里放几个用户（NodeInboundSyncJob 的批大小）。
     |
     | 为什么默认是 2，而不是 1 或 10：
     | - 每个用户固定要发 1 次面板 attach，这个省不掉；批处理省的是「每个 Job 的固定开销」
     |   （队列取任务、按 item 更新 async_task_items、连同 attach 的那次 HTTPS 往返）。
     |   从 1 → 2 就把固定开销摊掉了一半，已经拿到大部分收益；
     | - 再往上（4/8/10）摊薄收益递减，代价却线性上升：单个 Job 的执行时间变长，
     |   更容易撞上 worker 的 --timeout（本机是 120 秒）；且一个用户把 Job 打成失败后，
     |   $tries 重试是【整批重来】，批越大、一个坏用户牵连的「无辜重试」越多。
     | 2 是收益与失败半径的折中点。允许 env 覆盖以适配不同面板的延迟。
     */
    'node_sync_batch' => max(1, (int) env('TASKS_NODE_SYNC_BATCH', 2)),
];
