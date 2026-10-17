# ControlHub

3x-ui 订阅管理中枢 | 3x-ui Subscription Management Hub

**版本：1.33.0**

<!-- PROJECT SHIELDS -->
[![Version][version-shield]][version-url]
[![License][license-shield]][license-url]
[![PHP][php-shield]][php-url]
[![Vue][vue-shield]][vue-url]

---

[English](#english) | 中文

## 界面预览

用户端首页：

![用户端首页](screenshots/home.png)

管理端 · 仪表盘（节点监控 / 3D 地球）：

![管理端仪表盘](screenshots/admin-dashboard.png)

管理端 · 节点管理：

![管理端节点管理](screenshots/admin-nodes.png)

## 一键安装

```bash
curl -fsSL https://raw.githubusercontent.com/YouzSpace/3xui-hub/main/install.sh | bash
```

安装完成后使用 `3hub` 命令管理系统。

## Docker 部署

```bash
git clone https://github.com/YouzSpace/3xui-hub.git
cd 3xui-hub/docker
docker-compose up -d
```

访问 http://localhost:8080

常用命令：
```bash
docker-compose logs -f      # 查看日志
docker-compose down          # 停止
docker-compose restart       # 重启
docker-compose up -d --build # 更新并重启
```

## 功能特性

### 管理员端

- 仪表盘：用户/收入统计、节点状态与实时指标
- 用户管理：增删改查、分配套餐、续费、重置流量、重置密码、切换协议
- 节点管理：3x-ui 面板节点 + xray 裸内核节点（agent 自动部署、自升级、实时指标）
- xray 节点配置：入站（VLESS / VMess / Trojan / Shadowsocks，TCP / WebSocket / gRPC，Reality / TLS）、出站、路由规则、WARP 出口
- Reality 目标检测：在节点上跑真实 TLS 握手测速，挑出可用且最快的伪装目标，支持多选填 serverNames
- 套餐管理：周期/总量套餐、价格设置、节点流量倍率、第三方订阅捆绑
- 订单与支付：多渠道支付网关、订单查询、支付回调
- 优惠码：批量生成、限次限时、邀请返利
- 邮箱配置：SMTP 设置、通知场景开关、批量群发、每月定时发信、发信日志
- 多域名：域名绑定、SSL 申请与自动续期、Nginx 配置助手
- 站点配置：站点名称、Logo、公告、自定义公开首页、后台配色
- 教程管理：分类分组、Markdown 内容编辑
- 系统状态、异步任务日志（失败可重试）
- 数据库备份：导出/导入、差异预览、增量合并/覆盖
- 安全设置：密码修改、谷歌二步验证（2FA）

### 用户端

- 登录：邮箱密码 / Token 登录，找回密码
- 注册：邮箱注册（图形验证码）
- 仪表盘：流量统计、订阅地址与二维码、节点列表、使用文档入口
- 订阅：Clash / Sing-box / Base64 三种格式，节点旗帜与重命名
- 订购订阅：选择套餐、在线支付、流量重置包
- 我的订单：独立页面查看历史订单与支付状态
- 系统公告：铃铛弹窗查看，发布新公告后进入即自动弹出（可「不再提示」）；「我的」页进入全屏公告页
- 使用文档：独立二级页面，按分类内容列表 + 详情阅读
- 邀请好友：邀请码与返利记录
- 反馈链接：问题反馈入口

### 自动化

- 流量自动同步：每5分钟自动同步所有用户流量
- 超量/到期自动关流量：只关 3x-ui 流量、永不封禁账号，用户仍可登录续费
- 自动通知：到期 / 超量 / 即将超量，同一用户同一场景只提醒一次，重新购买或重置流量后恢复提醒
- 队列分流：节点类任务走 `node-ops`、邮件类走 `mail`，都不阻塞封禁/健康检查等定时任务（见 [INSTALL.md 队列与并发](INSTALL.md#队列与并发)）
- 节点 agent 自升级、php-fpm 并发自动调优
- 3hub 命令：一键管理系统

## 3hub 管理命令

```bash
3hub status        # 查看系统状态
3hub check-update  # 检测更新
3hub update        # 更新系统
3hub admin-user    # 修改管理员账号
3hub admin-pass    # 修改管理员密码
3hub sync          # 手动同步流量
3hub sync-status   # 查看自动同步状态
3hub backup        # 数据库备份（导出到 /var/backups/3xui-hub，保留 7 份，每天 03:30 自动备份）
3hub log           # 查看错误日志
3hub restart       # 重启服务
```

## 技术栈

| 层级 | 技术 |
|------|------|
| 后端 | Laravel 13 + PHP 8.3+ + MySQL |
| 前端 | Vue 3.5 + Vite 8 + Pinia 3 + Tailwind CSS 3 |
| 动画 | GSAP 3 |
| 认证 | Session（管理员）+ Bearer Token（用户） |
| 支付 | MD5 签名对接第三方支付网关 |
| 2FA | Google Authenticator (TOTP) |

## 目录结构

```
3xui-hub/
├── install.sh          # 一键安装脚本
├── uninstall.sh        # 卸载脚本
├── 3hub                # 管理命令
├── README.md
├── INSTALL.md
├── docker/
│   ├── Dockerfile
│   ├── docker-compose.yml
│   ├── nginx.conf
│   ├── php.ini
│   ├── www.conf
│   ├── supervisord.conf
│   └── entrypoint.sh
├── backend/
│   ├── app/
│   │   ├── Console/        # 定时任务
│   │   ├── Drivers/        # 驱动抽象层（3x-ui / xray）
│   │   ├── Http/           # 控制器
│   │   ├── Jobs/           # 队列任务
│   │   ├── Listeners/      # 事件监听
│   │   ├── Models/         # 数据模型
│   │   ├── Providers/      # 服务提供者
│   │   ├── Services/       # 业务服务
│   │   ├── Traits/         # 特性
│   │   └── WebSocket/      # 节点 WS 长连接服务
│   ├── config/
│   ├── database/
│   │   └── migrations/     # 数据库迁移
│   ├── public/             # 网站根目录（部署时复制 frontend/dist）
│   ├── resources/
│   ├── routes/
│   │   ├── api.php         # API 路由
│   │   └── web.php         # Web 路由
│   └── storage/
└── frontend/
    └── dist/               # 前端构建产物
        ├── assets/         # JS/CSS
        ├── icons.svg
        ├── images/
        └── index.html
```

## 版本记录

### v1.33.0 (2026-10-17)

- **Geo 数据（geoip.dat / geosite.dat）自动更新与分发**：面板每周一自动从上游检查更新并分发到所有节点，节点侧 sha256 校验、有变化才重载内核；新增「Geo 数据」管理页（文件状态 / 各节点同步状态 / 检查更新 / 手动分发），agent 升级至 1.0.5
- **入站嗅探（sniffing）配置开放**：启用 / 协议（HTTP/TLS/QUIC/FakeDNS）/ 仅路由（routeOnly）可在入站表单配置，默认值与现状一致（不配置无感知）
- **socks/http 出站 UDP 兼容护栏**：规则把 UDP（如 DNS）送进不支持 UDP 的 socks/http 出站时，自动回落直连 —— 修复客户端「延迟正常但打不开网页」的黑洞问题；规则列表加「UDP→直连」标注、路由页加「实际下发规则预览」、保存时主动提示
- **Reality 目标扫描修复**：扫描填入改为「所选目标证书的 serverNames 全集」（单选），杜绝跨证书混填导致 REALITY 握手失败；serverNames 上限 8 → 64 并过滤通配符
- **用户管理「保存」立即返回**：换套餐的节点同步改后台异步执行，保存按钮带 loading 防重复提交（此前节点多 / 掉线时要等十几秒）
- **弱网优化**：管理接口超时 15s → 30s，幂等 GET 请求网络抖动自动重试一次
- **Nginx 文本资源 gzip**：JS/CSS/JSON 开启压缩传输（弱网下体积约省 60~75%），新装与升级自动生效

### v1.32.0 (2026-10-14)

- **后台「监控」页并入仪表盘**：新增「最近 24 小时流量」全机汇总曲线（上下行速率实线左轴 + 累计上传/下载虚线右轴，悬浮显示四值）、「流量消耗 Top 5」（含峰值速率与时刻）、「平均 CPU / 内存占用排行」（含峰值与时刻），两栏布局；原独立「监控」页撤掉、旧链接自动跳回仪表盘
- 曲线汇总在**后端按同一时间桶归并**：多节点采样时刻的秒级差不再产生「交错锯齿」假峰谷（24h 按 5 分钟桶、7d/30d/本月按小时/天桶）
- 修复：低流量速率显示「N undefined/s」（格式化函数单位索引未钳下界）

### v1.31.0 (2026-10-14)

- 新增后台**「监控」页**：机器流量合计卡、TOP 排行、速率曲线（最近 24 小时 / 7 天 / 30 天 / 本月）、CPU·内存均值与峰值、明细表 + CSV 导出 —— 采样随每 5 分钟健康检查顺路完成、零额外请求；原始采样保留 90 天、每天 04:30 自动折算永久留存
- xray 节点**机器网卡流量采集**（agent 升级至 1.0.4）：上报累计收发总量、实时上下行改按网卡口径（此前依赖内核 stats、实测长期为 0）；旧 agent 不受影响、面板自动推送升级
- **应用日志按天轮转**（保留 14 天）：升级时自动归档历史大日志，`3hub log` 直接查看最新日志
- **数据库备份改造**：`3hub backup` 与每日 03:30 自动备份统一写入 `/var/backups/3xui-hub`，保留最新 7 份、失败不留半截文件
- **`3hub update` 升级失败自动回滚**：升级前自动记录代码快照 + 数据库备份，依赖安装 / 数据库迁移 / Worker 启动失败时自动退回升级前版本，不再停在中间态

### v1.30.0 (2026-10-14)

- 新增 **HTTPUpgrade**、**TCP HTTP 伪装**传输：入站/出站表单、订阅链接、Clash / Sing-box 订阅、跨机中转全链路支持
- **xhttp 高级字段**（xPadding / sessionID / xmux 等）开放配置，全链路透传
- 修复：xhttp 订阅链接断链（缺 path / host / mode）、TCP 伪装入站的链接地址被伪装域名污染、跨机中转的 xhttp 参数丢失
- 修复：Reality 中转出站对接 3x-ui 时自动推导公钥 —— 此前生成缺公钥的坏配置会让节点**所有配置变更卡住**（保留旧配置不再下发）
- 组合校验前后端同守：Reality 仅限 VLESS × tcp / xhttp / gRPC；TLS 入站强制证书；shortId 强制十六进制 —— 错误组合会被内核拒绝加载整份配置，现在保存时直接拦截
- Reality 入站传输面放开 xhttp / gRPC（此前表单误把非 tcp 传输整体禁用）

### v1.29.1 (2026-10-13)

- 修复 issue #20：中转出站「建得出、编辑不了」（点编辑再保存报 `The selected network is invalid. (and 1 more error)`）
  - 出站/入站的 `network`、`security` 校验白名单补齐 `xhttp` 与 `reality`；结构化表单补上对应分支（xhttp 的 path/host/mode，reality 的公钥/SNI/shortId/指纹/spiderX）
  - 出站页传输/安全层下拉同步补齐 `xhttp` / `Reality`（否则编辑时两项显示空白、保存即被拒）
- 修复中转出站默认标识重复端口：入站 tag 已含端口时不再重复拼（`JP1-43981-in-43981-tcp` → `JP1-in-43981-tcp`）
- 出站列表「目标」列：无备注时显示「服务器名 · 端口」，不再回落到标识导致与「标识」列重复

### v1.29.0 (2026-10-13)

- 出站页「本站服务器」候选显示补全：3x-ui 节点也显示完整的「服务器 · 端口 · 入站名 · 协议」（打开页面时实时读取对方面板 + 5 分钟缓存；读不到自动降级、不报错）
- 中转出口的**标识与备注可自定义**：标识默认「节点名-端口-入站名」（允许中文），备注默认留空（填了在列表显示）；标识与已有出站重名直接报错
- 出站列表新增**「复制链接」**：把出站导出为客户端配置链接（vless / vmess / trojan / ss / socks / http），可直接粘贴到 3x 面板当出口用；wireguard 类型不支持（按钮置灰）
- 措辞统一：出站页目标来源为「自己填写（手填地址、端口、凭据）」/「本站服务器（从已接入的服务器里选，自动配好出口）」

### v1.28.0 (2026-10-13)

中转流程重构：**出口先在出站页建，路由只选已有出站**（职责更清楚，不再在保存路由时隐式创建出站）：

- 出站页新建出站时，目标来源可选「其它机器」：选服务器 + 入站即自动生成出站（地址/端口/凭据/reality·xhttp 等传输参数全部自动读取），并在对方服务器上自动建好承接凭据
- 路由页「目标出站」只列本节点已有出站，末尾保留「＋ 新建中转出口」入口（点击跳转到出站页）
- 删除路由规则时自动回收它引用的中转出站（确认无其它规则引用后），并清理对方服务器上的承接凭据；删除中转出站同样清理对方凭据
- 管理后台措辞：侧边栏与页面「节点列表」改为「节点服务器」

### v1.27.1 (2026-10-13)

- 修复「跨机中转」到 3x-ui 目标节点时不通（现象：测出口 IP 不是目标机器的 IP）：
  - 取客户端凭据时未剥去 3x-ui 回执外层的 `client` 包裹，导致中转出站里始终没有写入 uuid（trojan 目标还会漏掉 password）
  - 中转出站的传输参数一直是空的，现按目标入站配置生成（reality / xhttp / tls / ws / grpc）
  - 已存在的坏中转出站，在再次保存对应路由规则时自动刷新修正

### v1.27.0 (2026-10-12)

xray 跨机中转与运维体验：

- 路由「目标出站」新增「其它机器」组：直接选别的节点 + 具体入站作为出口，保存时自动在源节点建中转出站、在目标节点建对应凭据（xray 节点走渲染注入、3x-ui 节点走面板 API），重复保存自动复用
- 显示统一「节点名-端口-端口名」（如 `香港01-14443-vless-reality`）：路由来源入站、目标出站、出站列表三处都按这个格式显示，不再出现认不出的裸 tag（rl-*）；3x-ui 机器的入站端口/tag 在线时自动取（缓存 5 分钟）
- WARP 合并进出站：出站页新建时协议可直接选 WARP（自动注册 + 应用生成 wireguard 出站）；路由目标候选里固定显示「WARP」
- 定时任务减负：未启用 WARP 的机器不进 WARP 换 IP 调度；WARP 换 IP 频率每分钟改为每 5 分钟；邮件通知 / 订阅刷新 / 第三方抓取未配置时不再空转
- `3hub update` 两段式：先换代码再 exec 新脚本执行后半段 —— 本次新增的自愈动作当次生效（此前要再跑一次 update 才生效）
- 节点安装命令优化：去掉多余的 `--no-auto-deps` 兼容开关；命令改为「先下载脚本再执行」，避免管道中断时执行到半截

### v1.26.0 (2026-10-12)

xray 出站 / 路由 / WARP / Reality 能力补齐：

- 出站「中转」：可指定另一出站作为出口（`sockopt.dialerProxy`），表单下拉选择
- 出站支持粘贴分享链接导入（vless / vmess / trojan / ss / socks / http）
- 出站订阅（服务器池）：粘贴订阅地址即得一组出口；标签跨刷新稳定、SSRF 防护、定时自动刷新
- 路由：新增负载均衡（balancers：轮询 / 最低延迟 / 最低负载）+ 域名解析策略可配 + 规则命中测试器
- WARP：注册后自动应用（一步生成 wireguard 出站），随即可在路由中被选择
- Reality 入站：补齐 12 项高级参数（xver / spiderX / maxTimediff / mldsa65 抗量子 / fallback 限速等）；订阅链接指纹不再写死
- 节点安装脚本：state.json 原子写入、重复安装跳过重下（幂等）、缺失依赖自动安装

### v1.25.4 (2026-10-12)

- 修复卸载脚本漏清 `3xui-hub-queue-mail` / `3xui-hub-node-ws` 服务：卸载后面板机不再残留重启循环与僵尸服务；多域名助手（3hub-domain + sudoers）也随卸载一并清理
- xray 节点注册时自动回填连接地址（host 为空且来源为公网 IP 时；不覆盖已有值）：修复节点未手填地址导致用户订阅为空的问题；订阅生成不出任何链接时给出明确提示，不再静默返回空内容

### v1.25.3 (2026-10-12)

- 修正：v1.25.1 的「CLI opcache 默认开」回退为关闭 —— 实测 OPcache 共享在 systemd 独立 worker 间不成立，开启反而每进程多占约 7MB（2 核机型 6 个常驻进程合计约 34MB）；已装 v1.25.1 的机器更新时自动清理残留配置

### v1.25.2 (2026-10-12)

- 修复 3x-ui 面板节点「内存」读不到的问题（3x-ui 内存字段的对象格式未正确解析；CPU 不受影响）
- 仪表盘「上下行速率」曲线改为始终连续：无数据时段用直线连接，不再出现断口

### v1.25.1 (2026-10-12)

- 小内存机器（1 核 / 1GB 档）开箱即适配，无需手动调优：
  - 节点任务 Worker 数量按内存自适应：小于 1GB 的机器只开 1 个（省约 40~80MB），老机器升级后自动收敛
  - php-fpm 并发推荐值不再被硬性抬到 10：1 核小机器按实际容量推荐 4，避免极端占用把数据库挤爆
  - 常驻进程默认开启 opcache 共享缓存（每个进程约省 30MB）
- 更多说明见 INSTALL.md「小内存机器」一节

### v1.25.0 (2026-10-11)

- 仪表盘改版：新增节点监控 —— 3D 地球（拖拽旋转、点击地区定位）、实时 CPU / 内存 / 连接数 / 上下行曲线、节点卡片网格
- 业务概览新增「今日成交额」（按北京时间日界统计已支付订单）
- 修复订阅节点名的地区识别误判（如 Australia 被识别成美国、Netherlands 被识别成泰国）
- 节点列表接口查询优化（仪表盘 5 秒轮询开销显著下降）

### v1.24.1 (2026-10-11)

- 修复节点 config 接口的 ETag 协商：经 CDN 压缩后弱 ETag（W/"…"）永不命中 304，agent 每 30 秒误判「配置已更新」重启内核
- 修复 agent 内核进程记账错位：孤儿内核占住端口导致 exit 255 无限重启循环，只能重启 agent 复位；agent 升级至 1.0.3

### v1.24.0 (2026-10-10)

- xray 入站新增 Reality 目标检测：在节点上跑真实 TLS 握手（TLS1.3 / h2 / X25519 / 证书链受信任），
  弹窗表格按「可用 → 延迟」排序，勾选多个填入 serverNames、最快的作为 dest
- 邮件拆独立 `mail` 队列，群发不再堵塞封禁/健康检查等定时任务；发送失败自动释放防重标记并补发
- 邮件通知去重改为「每用户每场景一次」，重新购买 / 重置流量后恢复提醒
- 节点 agent 升级至 1.0.2（面板「节点」页一键升级）

### v1.23.2 (2026-10-10)

- 节点 agent 支持自升级；php-fpm 并发按机器规格自动调优
- 修复节点上报流量超过 2000 条被静默丢弃、订阅生成串行、建号阻塞

### v1.23.1 (2026-10-09)

- 邮箱自动通知改为每自然月一封；无套餐账号不再收信

### v1.23.0 (2026-10-08)

- xray 裸内核节点：agent 自动部署、指令秒级下发、实时指标上报
- 邮箱自动通知修复

### v1.22.0 (2026-10-07)

- 第三方机场订阅；邮箱页改造；管理后台配色

### v1.21.0 (2026-10-05)

- 自定义公开首页；修复套餐有效期超过 2038 年写入失败

### v1.11.0 (2026-09-25)

- 用户端：使用文档独立为全屏二级页面，「我的」页与仪表盘入口均跳该页
- 用户端：弹层改为上下居中，内容超高时弹层内部滚动，去掉拖拽把手
- 用户端：系统公告改为弹窗查看，发布新公告后进入即自动弹出，支持「不再提示」
- 用户端：「我的」页保留全屏公告页入口，公告页去掉重复标题

### v1.7.0 (2026-09-18)

- 新节点接入改为按用户异步初始化，单个用户失败不阻塞其他用户
- 增加初始化任务进度、失败重试和节点启用状态保护
- 修复异步任务超时后的迟到回执，避免任务状态被重新改写
- 修复 3x-ui 客户端更新参数和全量删除语义
- 修复多入站配置比较和空入站校验
- 流量同步继续使用按 email 去重、批量快照和批量流量更新
- 管理员任务列表增加节点接入初始化类型

### v1.2.1 (2026-07-01)

- Logo 背景色统一使用 CSS 变量
- 所有 Logo 添加 :has(img) 处理，SVG 图片背景透明
- 右下角显示版本号

### v1.2.0 (2026-07-01)

- 教程发布系统：分类分组、Markdown 内容编辑
- 公告管理：发布/编辑/删除公告
- 用户后台重构：多页导航 + UI 优化
- 邮箱验证注册 + 注册同步 3x-ui
- 站点配置读取修复
- 反馈链接入口

### v1.1.0 (2026-06-27)

- 流量同步性能优化：批量拉取 + 并行请求 + upsert 快照
- 支持万级用户 + 千级节点
- 快照表改为 upsert 模式（每 user+node 只保留1条）

### v1.0.0 (2026-06-24)

- 初始发布
- 用户管理（注册/登录/邮箱/Token）
- 节点管理（多入站/测试连接）
- 套餐管理（周期/总量/价格）
- 支付系统（多渠道/下单/回调/订单）
- 订阅系统（Base64/流量同步/封禁）
- 安全功能（密码修改/谷歌二步验证）
- 备份管理（导出/导入/差异预览）
- 流量自动同步
- 3hub 管理命令
- 一键安装脚本

## 许可证

MIT License

---

# English

# ControlHub

3x-ui Subscription Management Hub — A centralized platform for managing nodes, users, plans, and payments.

**Version: 1.33.0**

## Screenshots

Landing page:

![Landing Page](screenshots/home.png)

Admin dashboard (node monitoring / 3D globe):

![Admin Dashboard](screenshots/admin-dashboard.png)

Admin · Nodes:

![Admin Nodes](screenshots/admin-nodes.png)

## Quick Install

```bash
curl -fsSL https://raw.githubusercontent.com/YouzSpace/3xui-hub/main/install.sh | bash
```

After installation, use `3hub` command to manage the system.

## Docker Deploy

```bash
git clone https://github.com/YouzSpace/3xui-hub.git
cd 3xui-hub/docker
docker-compose up -d
```

Visit http://localhost:8080

Common commands:
```bash
docker-compose logs -f      # View logs
docker-compose down          # Stop
docker-compose restart       # Restart
docker-compose up -d --build # Update and restart
```

## Features

- User / node / plan / order management
- Panel nodes (3x-ui) and bare-kernel xray nodes with an auto-upgrading agent
- xray inbounds (VLESS / VMess / Trojan / Shadowsocks, Reality / TLS), outbounds, routing, WARP
- REALITY target scanner: real TLS handshake on the node, pick the fastest usable target
- Multi-channel payment gateway, discount codes, referrals
- Email notifications (expiry / quota), batch sending, monthly scheduled mail, delivery logs
- Multi-domain with SSL issuance & renewal helper
- Clash / Sing-box / Base64 subscriptions
- Auto traffic sync; over-quota users lose traffic only — accounts are never banned
- Queue isolation (`node-ops` / `mail`) so batch work never blocks scheduled jobs
- Google 2FA, database backup & migration
- 3hub CLI management tool

## Tech Stack

- Backend: Laravel 13 + PHP 8.3+ + MySQL
- Frontend: Vue 3.5 + Vite 8 + Pinia 3 + Tailwind CSS 3
- Animation: GSAP 3
- Payment: MD5 signed third-party gateway
- 2FA: Google Authenticator (TOTP)

## License

MIT License

<!-- LINKS -->
[version-shield]: https://img.shields.io/badge/version-1.33.0-blue
[version-url]: #
[license-shield]: https://img.shields.io/badge/license-MIT-green
[license-url]: #许可证
[php-shield]: https://img.shields.io/badge/PHP-8.3+-777BB4
[php-url]: https://php.net
[vue-shield]: https://img.shields.io/badge/Vue-3-4FC08D
[vue-url]: https://vuejs.org
