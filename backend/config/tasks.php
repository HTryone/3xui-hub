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
];
