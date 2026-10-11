# 3xui-hub 安装指南

## 一键安装

```bash
curl -fsSL https://raw.githubusercontent.com/YouzSpace/3xui-hub/main/install.sh | bash
```

安装过程中：
1. 自动安装 PHP 8.4、Composer、Nginx
2. 输入域名或 IP（直接回车使用 IP）
3. 选择是否开启 SSL（y/N）

安装完成后自动显示访问地址和默认账号。

## 3hub 管理命令

安装后可使用 `3hub` 命令管理系统：

```bash
3hub status        # 查看系统状态
3hub check-update  # 检测更新
3hub update        # 更新系统
3hub admin-user    # 修改管理员账号
3hub admin-pass    # 修改管理员密码
3hub sync          # 手动同步流量
3hub sync-status   # 查看自动同步状态
3hub backup        # 导出数据库备份
3hub log           # 查看最近错误日志
3hub restart       # 重启 PHP-FPM 和 Nginx
3hub help          # 显示帮助
```

## 队列与并发

面板的后台任务跑在三条独立的队列上：

| 队列 | 跑什么 | Worker |
|------|--------|--------|
| `default` | 定时任务类：封禁检查、健康检查、流量同步 | `3xui-hub-queue.service` |
| `node-ops` | 节点类：新建节点初始化、节点 inbound 扫描/同步、客户端清理 | `3xui-hub-queue-node@.service`（实例按内存自适应，默认 1~2 个） |
| `mail` | 邮件类：自动通知、批量群发、每月定时发信 | `3xui-hub-queue-mail.service` |

### 为什么要多条队列

一条 `php artisan queue:work` 进程同一时刻只跑一个 Job（单线程），两类长任务都会把定时任务堵住：

- **节点任务**：新建节点时要初始化 100+ 个 Job，挤在同一条队列里，封禁检查和健康检查
  就得排在它们后面，可能延迟几分钟到几十分钟 —— 超流量用户在该封禁的时候还在跑。
- **邮件任务**：发信是 IO 等待型（每封 1~3 秒都在等 SMTP），且批量发信默认不限速 ——
  一次 500 人群发会让 worker 连续忙十几分钟，同样把 5 分钟定时任务堵在后面。

拆开后各排各的：节点任务、群发再长也不会挡住定时任务。代价是多几个常驻 PHP 进程。

### 两边必须都到位

应用侧「派到哪条队列」由 `.env` 的 `PANEL_NODE_OPS_QUEUE` / `PANEL_MAIL_QUEUE` 决定，
worker 侧由 `queue:work --queue=node-ops` / `--queue=mail` 决定，**两边都到位才生效**：

- 只改 `.env` 没起 worker → 对应任务被派进一条没人消费的队列，**永远不执行**，而且
  不报错（面板上一直停在 pending，直到被判超时失败）。
- 只起 worker 没改 `.env` → 任务仍进 `default`，多余的 worker 空转占内存。

`install.sh` 和 `3hub update` 都会同时做这两件事，并在启动后自检
（`install.sh` 查 `is-active` + `is-enabled`：前者证明现在在跑，后者保证重启后还在）：
起不来的话直接报错中断，而不是留下一个静默坏掉的面板。

### 调整 worker 数量

node-ops 实例数**按机器内存自适应**，不需要手动数并发：

| 内存 (MemTotal) | node-ops 实例 |
|-----------------|---------------|
| < 1024MB        | 1             |
| ≥ 1024MB        | 2             |

`mail` 固定 1 个（发信是 IO 等待型，一条 worker 的吞吐已超过常见 SMTP 服务商的限速，
多开只会互相抢同一个发信账号）。

要显式指定实例（内存够但就想省一个进程，或反过来加到 3 个），写覆盖文件
`/etc/3xui-hub/node-queue-instances`，内容是空格分隔的实例号：

```bash
# 只开 1 个
mkdir -p /etc/3xui-hub && echo 1 > /etc/3xui-hub/node-queue-instances
# 开 3 个
echo "1 2 3" > /etc/3xui-hub/node-queue-instances
# 删掉覆盖文件 → 回到按内存自适应
rm -f /etc/3xui-hub/node-queue-instances
```

写好后运行一次 `3hub update`（或重跑 `install.sh`）即生效：脚本会 enable 名单里的实例，
并把「已 enabled 但不在名单里」的实例 `systemctl disable --now` **收敛**掉 —— 老机器
升级后从 2 个减到 1 个时，残留的 `@2` 就是这个路径停掉的（不会继续白占内存）。
`3hub status` 的「节点 Worker」一行按实际实例数显示，例如 `运行中（node-ops，1/1）`。

Docker 部署改 `docker/supervisord.conf`：照着加一段 `[program:queue-node-3]`，
或者把 `queue-node-2` 整段删掉只留一个。

### 小内存机器（< 1GB）默认已按内存收敛

1 核 / 937MB 这类机器装完不用手动做任何事，三处默认值都已朝省内存调整：

- **node-ops 实例自适应**：MemTotal < 1024MB 时只开 1 个 node-ops worker（否则 2 个），
  少一个常驻进程约省 40~80MB。老机器升级上来若之前是 2 个，会被自动收敛到 1 个。
- **php-fpm 并发下限已修正**：面板的 `pm.max_children` 推荐值不再被死抬到 10。下限 = 
  `min(10, 按内存算, 按核数算)`、最低 2 —— 1 核/937MB 算出 4，避免 10×50MB≈450MB 把
  MySQL 挤到 OOM。可用 `php artisan fpm:tune` 查看/应用。
- **CLI opcache 已默认开**：安装/更新时给 php-cli 写 `99-3xui-hub-opcache.ini`
  （`opcache.enable_cli=1`），常驻 worker / 调度不再各自重新编译整套 Laravel，
  每个进程约省 30MB。

想再挤出内存，可以把邮件任务退回 default 队列，省掉一个常驻 worker（**约 60MB**）：
在 `backend/.env` 里把 `PANEL_MAIL_QUEUE=mail` 改成 `PANEL_MAIL_QUEUE=default`（或删掉该行），
然后 `systemctl disable --now 3xui-hub-queue-mail.service`。代价是邮件（尤其群发）会回到
default 队列 —— 一次大群发期间，封禁/健康检查这类 5 分钟定时任务可能排队延迟，
用「省内存」换「定时任务及时性」，小机器上通常值得。

### 注意：并发要顾着面板本身的承载

节点任务每个 Job 都要跨境请求 3x-ui 面板 API（实测 RTT 700~900ms，偶发 TLS 卡死），
所以瓶颈往往不在本机 CPU，而在**被操作的节点面板**和本机内存：

- **1 核 / 473MB 这类小机器**：就用默认的 2 个，甚至减到 1 个。一个 PHP worker 常驻
  约 40~80MB，多开几个很容易把 MySQL 挤到 OOM。两个 node-ops worker 已设 `Nice=10` +
  `CPUWeight=20`，让 Web 请求优先，但没有设内存硬上限（硬上限会在内存吃紧时杀掉
  跑到一半的节点任务，反而更糟）；default worker 保持原样，未加优先级设置。
- **被操作的面板是弱鸡机器（1 核 / 小带宽）时不要加并发**：同时打过去的初始化请求会
  把面板自己打满，整体反而更慢甚至假死。加并发只会放大问题，正确做法是错开时间
  （别在节点刚上线时批量操作）。
- 判断依据：`3hub status` 看内存；`3hub log`（含 worker 的 journal）看 worker 有没有
  被 OOM kill 或反复重启。

## 默认账号

- **管理员**: admin / admin123
- 首次登录后请立即修改密码！

## 手动安装

如需手动安装，参考以下步骤：

### 环境要求

- PHP 8.4+（扩展：fileinfo、pdo_mysql、mbstring、gd）
- Composer 2.2+
- Nginx

### 步骤

```bash
# 1. 克隆项目
git clone https://github.com/YouzSpace/3xui-hub.git /www/wwwroot/3xui-hub

# 2. 配置后端
cd /www/wwwroot/3xui-hub/backend
cp .env.example .env
php artisan key:generate
mysql -u root -e "CREATE DATABASE IF NOT EXISTS controlhub CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;"
php artisan migrate --seed

# 3. 复制前端到 public
cp -r ../frontend/dist/* public/

# 4. 设置权限
chmod -R 755 storage bootstrap/cache
chown -R www:www storage database

# 5. 配置 Nginx（参考下方配置）

# 6. 配置 cron（流量自动同步）
crontab -e
# 添加: * * * * * cd /www/wwwroot/3xui-hub/backend && php artisan schedule:run >> /dev/null 2>&1

# 7. 配置三条队列 Worker（systemd）
#    .env 里加 PANEL_NODE_OPS_QUEUE=node-ops 和 PANEL_MAIL_QUEUE=mail
#    default 队列： /usr/bin/php artisan queue:work database --sleep=2 --tries=5 --timeout=120 --max-time=3600
#    node-ops 队列：/usr/bin/php artisan queue:work database --queue=node-ops --sleep=2 --tries=5 --timeout=120 --max-time=3600
#    mail 队列：    /usr/bin/php artisan queue:work database --queue=mail --sleep=2 --tries=5 --timeout=120 --max-time=3600
#    三条都要，缺哪条 worker，对应任务就永远不执行（见「队列与并发」）。
#    单元文件模板可直接抄 install.sh 里的 setup_queue_worker / setup_node_ops_worker / setup_mail_worker
```

### Nginx 配置示例

```nginx
server {
    listen 80;
    server_name your-domain.com;
    root /www/wwwroot/3xui-hub/backend/public;
    index index.php index.html;

    location ~ [^/]\.php(/|$) {
        fastcgi_pass unix:/run/php/php8.4-fpm.sock;
        fastcgi_index index.php;
        include fastcgi_params;
        fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;
    }

    location ~ ^/(api|admin-api) {
        try_files $uri $uri/ /index.php?$query_string;
    }

    location / {
        try_files $uri $uri/ /index.html;
    }

    location ~* \.(env|git) {
        return 404;
    }
}
```

## 常见问题

### 安装失败
查看安装日志：`cat /tmp/3xui-hub-install.log`

### 流量不同步
检查 cron 是否配置：`3hub sync-status`

### 节点任务一直 pending / 不执行
节点类任务（新建节点初始化、节点扫描等）走独立队列 `node-ops`，先确认 worker 在跑：

```bash
3hub status        # 「节点 Worker」应为 运行中（node-ops，1/1 或 2/2，按内存自适应）
3hub log           # 含 worker 的 systemd journal，看有没有 OOM kill / 反复重启
```

若显示未运行，`3hub update` 会重新生成单元文件并启动它。也可能是 `.env` 里开了
`PANEL_NODE_OPS_QUEUE=node-ops` 却没有对应的 worker —— 两边必须同时到位，
详见「队列与并发」。

### 500 错误
查看日志：`3hub log` 或 `tail -20 /www/wwwroot/3xui-hub/backend/storage/logs/laravel.log`

### 忘记密码
```bash
3hub admin-pass
```
