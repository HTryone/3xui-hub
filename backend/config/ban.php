<?php

return [
    /*
     | 「已确认关闭」的时效（小时）。
     |
     | 用户被确认关闭 3x-ui 流量后，扫描器（BanCheckJob / SyncTrafficCommand）
     | 在该时间内直接跳过，不再为该用户发节点请求；超过该时间会重新校验一次，
     | 用于兜住「面板上被人工开回来」「关闭请求实际没落到某个节点」等状态漂移。
     |
     | 值越大 → 请求越少，但状态漂移的窗口越长；配 0 表示每轮都重新校验（关闭该优化）。
     | 写法参考 config/tasks.php 的 stale_after_minutes。
     */
    'recheck_after_hours' => (int) env('BAN_RECHECK_AFTER_HOURS', 6),
];
