<?php

return [
    /*
     | xray 节点 WebSocket 长连接服务（面板侧 Workerman 常驻进程）。
     |
     | 架构：agent 连 nginx 反代的 /node-ws → 转发到本服务（只绑回环）。
     | agent 连不上时自动降级 HTTP 六端点通道，业务不中断。
     */

    // Workerman 监听地址（只绑回环，经 nginx 反代对外）
    'ws_listen' => env('XRAY_WS_LISTEN', '127.0.0.1:8091'),

    // 是否向节点下发 WS 连接信息（关闭后节点一律走 HTTP 降级通道）
    'ws_enabled' => (bool) env('XRAY_WS_ENABLED', true),

    // 对外路径（nginx location 与此一致）
    'ws_path' => env('XRAY_WS_PATH', '/node-ws'),

    // 下发给节点的 WS 完整地址覆盖（留空 = 按请求 Host 拼 wss://host/node-ws；
    // 本地联调可设 ws://127.0.0.1:8091 直连服务）
    'ws_public_url' => env('XRAY_WS_PUBLIC_URL', ''),

    /*
     | node_api_commands（面板 → 节点的指令队列）终态回执保留天数。
     |
     | 这张表只有写入与轮询，没有消费方（面板不展示历史指令），所以过期回执
     | 纯占盘：线上实测 ≈175 行/天（主要是增删用户产生的 adu/rmu），且会被
     | 每日备份按份数放大。清理是「懒删除」——跟着回执落库顺带做，不另加定时任务：
     | 不同步/无回执就不写，自然不需要清。
     |
     | 只删 success / failed 且已过期的行；pending / acknowledged 一律不动
     | （还可能被 agent 取走或在途）。设为 0 关闭清理。
     */
    'command_retain_days' => (int) env('XRAY_COMMAND_RETAIN_DAYS', 30),
];
