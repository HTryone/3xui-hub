# 驱动层（P5：3x-ui + xray）

逐条对齐 `backend/app/Drivers/**` 与 `backend/app/Services/ThreeXUi/**`。**不含 payment 驱动**（P7 负责）。

## 包结构

```
internal/driver/
├── threexui/            3x-ui v3.x 面板 API 客户端 + 驱动
│   ├── client.go        HTTP 底层：Bearer / cookie+CSRF 登录、登录态缓存、{success,obj} 信封
│   ├── api.go           端点方法（clients / inbounds / server / 批量流量）
│   ├── driver.go        panel.Driver 契约实现 + ClientEmail()
│   ├── errors.go        四类错误（业务 / not-found / 会话失效 / 传输层）
│   ├── node.go          NewForNode：nodes 行 → 客户端（password/api_key 用 crypto 解密）
│   └── testdata/        gen_golden.php + golden_threexui.json（PHP 真实现产出）
├── xray/                xray 裸内核节点
│   ├── config.go        全量 config 渲染（入站/clients/policy）
│   ├── routing.go       出站/路由/balancer/observatory 渲染
│   ├── subscription.go  订阅 URI 生成 + serverNameFor / sameSite
│   ├── relay.go         中转出站决策与组装
│   ├── reality.go       X25519 密钥对 / shortId / 私钥推公钥
│   ├── targets.go       Reality 目标候选目录
│   ├── warp.go          Cloudflare WARP 注册 + wireguard 出站
│   ├── driver.go        xray 驱动（指令队列 + 心跳健康检查）
│   └── testdata/        gen_golden.php + golden_xray.json
└── internal/            仅 driver 下可引用
    ├── panel/           驱动契约与能力常量（对齐 Capability / PanelDriverInterface）
    ├── phpjson/         三套 PHP json_encode 口径 + 保序解析
    ├── phpval/          PHP 标量语义（(int)/(string)/empty()/?? ）+ rawurlencode
    └── secrets/         Laravel 密文字段解密（严格 / 宽松两套失败口径）
```

`internal/` 放共享小包而不是复制：驱动层最容易写歪的就是「散落的隐式类型转换」，
统一入口后调用点只写「我在复刻哪条 PHP 语义」。

## 验收方式

对拍基准全部由**真实 PHP 实现**产出，不是 Go 自转圈：

| 夹具 | 生成器 | 对拍内容 |
|---|---|---|
| `xray/testdata/golden_xray.json` | `gen_golden.php`（临时 SQLite，跑完即删） | config 逐字节、ETag、订阅 URI、adu 载荷、reality 推导、中转出站、WARP 出站 |
| `threexui/testdata/golden_threexui.json` | `gen_golden.php`（Guzzle MockHandler） | 每条请求的 method/path/query/header/body 语义、返回值、not-found 文案、authCacheKey、baseURL |
| `internal/secrets/testdata/fixtures.json` | `gen_fixtures.php` | 真实 `Crypt::encryptString` 密文 → Go 解密 |

重新生成（PHP 侧改动后）：

```bash
php server/internal/driver/xray/testdata/gen_golden.php
php server/internal/driver/threexui/testdata/gen_golden.php
php server/internal/driver/internal/secrets/testdata/gen_fixtures.php
cd server && go test ./internal/driver/... -v
```

## 与 PHP 的有意差异（逐条写清理由）

1. **登录态缓存**：PHP 用 Laravel cache（跨进程共享）；Go 侧用进程内 map + TTL，
   键（`panel-auth:sha1(...)`）与 PHP **逐字节相同**，接口 `AuthCache` 可注入，
   将来接 Redis 不用改 client.go。
2. **没有 async 方法**：PHP 的 `*Async()` 只是绕开 Guzzle `curl_multi` 的串行 wait；
   Go 用 ctx + goroutine，Bearer 模式天然无状态。
3. **请求体字段顺序**：PHP 的 `'json' => $arr` 按调用方插入顺序输出；Go 的 map
   无法保序（按 key 排序）。3x-ui 按 JSON 语义解析，**语义等价**（测试用语义比较，
   转义口径由 `phpjson` 单测锁死）。
4. **`Config.Port = 0`**：Go 视为「未设置 → 443」；PHP 的 `$config['port'] ?? 443`
   只在键缺失时回落，显式 0 会拼出 `https://h:0`。线上 `fromNode()` 恒传 DB 值
   （`nodes.port` 默认 443），该分支不可达。
5. **xray 驱动不持有数据库连接**：所有输入通过 `Source` 接口传入，便于单测；
   落库/行锁/`bumpXrayConfigVersion` 留在业务层（P6/P8b）。
6. **WARP 的 secretKey**：PHP 把它塞进 outbound settings 再在落库时抽出来加密；
   Go 的 `WarpSettings()` 把「可公开 settings」与「需加密私钥」分开返回，
   避免误把明文写进 `secret_key` 列。

## 已知风险（需要业务层/后续阶段处理）

1. **`ToggleClient` 的整条替换语义（PHP 现状，Go 照抄）**
   `ThreeXUiDriver::toggleClient()` 调 `updateClient($email, ['enable' => $enable])`，
   而 3x-ui 的 `/clients/update/{email}` 是**整条替换**——只带 `enable` 的 body 会把
   该 client 的 uuid/limit/expiry 清空。修复应在业务层（先 `GetClient` 取完整对象再改
   `enable`），驱动层保持与 PHP 一致以免两边行为分叉。**调用方必须先读后写。**

2. **`fingerprint()` 未在驱动层复刻**
   它用 MySQL 专属函数（`CRC32`/`CONCAT_WS`/`DATE_FORMAT`），且已由 P4
   `internal/nodeapi` 按 MySQL 夹具对拍过；驱动层只提供 `Render`/`ETag`。
   若后续在驱动层新增指纹计算，必须与本包 `RenderJSON` 的输出保持同源。

3. **`ss`（shadowsocks）入站不经用户级指令通道**
   `adu/rmu` 与订阅链接都按「用户协议 == 入站协议」过滤且排除 `shadowsocks`
   （单凭据模式）。业务层建号时不要指望给 ss 入站下发 adu。

4. **`adu` 载荷必须带该入站已有的中转凭据**
   `ADUInboundFragment(ib, user, relayClients, key)` 的第三个参数传空会让一次建号
   把中转凭据从内核里抹掉（PHP 的 `renderInbound` 无条件追加）。
   `Source.RelayClients()` 必须实现（驱动已按此调用）。

5. **HTTP 错误码 vs 连接失败的降级边界（照抄 PHP，容易写错）**

   | 情况 | PHP 异常 | `requestNullable` 结果 | Go 实现 |
   |---|---|---|---|
   | HTTP 4xx/5xx | `RequestException` → `ThreeXUiException` | **降级成 null** | `*Error`（被吞） |
   | 连不上/超时 | `ConnectException`（继承 `TransferException`） | **上抛** | `*TransportError`（穿透） |
   | 重登后仍 401 | `ThreeXUiException` | 降级成 null | `*Error`（被吞） |

   即：「节点返回 502」在 `GetClient` 里表现为「该用户不存在」，而「节点连不上」
   会一路抛到调用方。这是 PHP 现状，Go 侧逐条对齐；业务层若要区分，请看
   `errors.As(err, *TransportError)`。

6. **节点凭据解密入口**
   - 3x-ui：`threexui.NewForNode(NodeRow{...}, appKey)`（严格口径，坏密文即报错，
     对齐 `Node::password`/`api_key` accessor）。
   - xray：`xray.NodeSecret(driverConfigRaw, appKey)`（宽松口径，明文原样返回，
     对齐 `Node::nodeSecret()` / `NodeWarpAccount`）；`Render`/`RenderJSON` 内部
     解密 `reality_private_key` 与 `secret_key`，调用方只需传 `appKey`。
   - **不要在各业务模块自己解**：两套失败口径（严格/宽松）分叉是排查噩梦。

7. **`verify_ssl` 默认 false**（对齐 `ThreeXUiClient::fromNode()` 的
   `$node->verify_ssl ?? false`）。生产节点应显式打开；驱动层按节点配置执行，不做全局开关。
