# ============================================
# ControlHub - 单容器部署（Go 后端）
# 多阶段构建：golang 编译 → Debian + Nginx + MySQL + Supervisor 运行
# ============================================

# ── 阶段 1：编译 Go 后端 ──
FROM golang:1.25 AS builder
WORKDIR /build
COPY server/ ./server/
RUN cd server && CGO_ENABLED=0 go build -o controlhub-server ./cmd/server

# ── 阶段 2：运行镜像 ──
FROM debian:bookworm-slim

# 系统依赖（Nginx + MySQL + Supervisor + 节点安装脚本依赖的 curl/unzip）
RUN apt-get update && apt-get install -y \
    nginx \
    mysql-server \
    supervisor \
    curl \
    unzip \
    && rm -rf /var/lib/apt/lists/*

# 工作目录
WORKDIR /var/www/html

# Go 后端二进制 + 建表脚本（backend/ 目录保留放 .env，Go 用 CONFIG_ENV 指向它）
COPY --from=builder /build/server/controlhub-server /var/www/html/server/controlhub-server
COPY server/schema/ /var/www/html/server/schema/
# 前端构建产物（nginx 静态）
COPY frontend/dist/ /var/www/html/frontend/dist/
# 部署配置 + 管理命令
COPY docker/ /var/www/docker/
COPY 3hub /usr/local/bin/3hub

# 配置文件
RUN cp /var/www/docker/nginx.conf /etc/nginx/sites-available/default \
    && cp /var/www/docker/supervisord.conf /etc/supervisor/conf.d/supervisord.conf \
    && chmod +x /usr/local/bin/3hub /var/www/html/server/controlhub-server /var/www/docker/entrypoint.sh

# 数据目录
RUN mkdir -p /var/lib/mysql /var/run/mysqld /var/run/nginx /var/www/html/backend \
    && chown -R mysql:mysql /var/lib/mysql /var/run/mysqld

# 环境变量（Go 后端直连 MySQL；APP_KEY 由 entrypoint 生成）
ENV DB_HOST=127.0.0.1 \
    DB_PORT=3306 \
    DB_DATABASE=controlhub \
    DB_USERNAME=root \
    DB_PASSWORD= \
    APP_KEY= \
    APP_URL=http://localhost

# 端口
EXPOSE 80

# 入口点
ENTRYPOINT ["/bin/bash", "/var/www/docker/entrypoint.sh"]
