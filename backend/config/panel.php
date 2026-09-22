<?php

return [
    /*
     | 3x-ui 面板 API 超时（秒）。
     |
     | 跨境节点（HK 实测 RTT 699~903ms，间歇 TLS 卡死 10s+）下，原先写死的 2 秒
     | connect_timeout 会让节点初始化批量失败、流量同步误报，故全部改为可 env 覆盖。
     | 仅调超时，不动重试与并发行为。
     */

    // 常规 API 请求总超时（ThreeXUiClient 构造 Guzzle 客户端时的默认值）
    'api_timeout' => (float) env('PANEL_API_TIMEOUT', 15),

    // 常规 API 请求连接超时
    'connect_timeout' => (float) env('PANEL_CONNECT_TIMEOUT', 5),

    // 健康检查（/panel/api/server/status）总超时
    'healthcheck_timeout' => (float) env('PANEL_HEALTHCHECK_TIMEOUT', 8),

    // 健康检查连接超时
    'healthcheck_connect_timeout' => (float) env('PANEL_HEALTHCHECK_CONNECT_TIMEOUT', 5),

    /*
     | cookie 模式登录态缓存（秒）。
     |
     | 未配 api_key 的节点每次操作都要先 POST /login + GET /csrf-token；Web 请求与队列
     | Job 是两个进程，各自的实例内标记救不了对方，于是同一个面板会被反复登录。
     | 这里把登录后的 cookie + csrf token 按「面板指纹」缓存起来跨进程共用，
     | TTL 内同一个面板只登录一次。设为 0 表示关闭缓存（回退为每次登录）。
     */
    'auth_cache_ttl' => (int) env('PANEL_AUTH_CACHE_TTL', 300),

    /*
     | 登录 / 取 CSRF 的独立超时（秒）。
     |
     | 这两个请求是为了「拿到业务请求的门票」，不该继承业务请求 15 秒的上限：
     | 面板不可达时应尽快失败并给出人话提示，而不是把 15 秒耗在一个登录上。
     */
    'login_timeout' => (float) env('PANEL_LOGIN_TIMEOUT', 5),
    'login_connect_timeout' => (float) env('PANEL_LOGIN_CONNECT_TIMEOUT', 5),
    'csrf_timeout' => (float) env('PANEL_CSRF_TIMEOUT', 2),
    'csrf_connect_timeout' => (float) env('PANEL_CSRF_CONNECT_TIMEOUT', 2),

    /*
     | 节点类 Job（NodeInboundScanJob / NodeInboundSyncJob / NodeClientsCleanupJob /
     | NodeInitUserJob）投递到的队列名。
     |
     | **默认值必须是 'default'**，与引入本开关之前的行为逐字一致。
     | 理由：Job 被派到一条没有任何 worker 监听的队列 = 任务永远不执行，日志页会一直停在
     | pending，直到 async-task-timeout 把它判失败 —— 这比「节点任务和定时任务抢同一条队列」
     | 严重得多，而且是静默的。所以队列名只能由部署侧显式打开：
     | 在 .env 里设 PANEL_NODE_OPS_QUEUE=node-ops，并另起一个 worker 监听该队列
     | （php artisan queue:work --queue=node-ops ...），两边都到位才生效。
     | 只改一边（改了 .env 没起 worker，或起了 worker 没改 .env）时，默认值保证不会把任务派丢。
     */
    'node_ops_queue' => env('PANEL_NODE_OPS_QUEUE') ?: 'default',
];
