// Package store 负责数据库连接。统一使用 database/sql + go-sql-driver/mysql 手写 SQL，
// 不使用 ORM：金额（decimal:2）、布尔 tinyint、JSON 字段都要逐字段对齐 Laravel 的输出形态。
//
// 表结构由 Laravel migration 建好，Go 侧只读写不迁移。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"controlhub-server/internal/config"

	"github.com/go-sql-driver/mysql"
)

// 连接池参数。PHP-FPM 侧是"每请求一条连接"，Go 侧复用长连接，
// 因此上限必须收紧，避免把 MySQL 的 max_connections 吃满。
//
// 上限由 config.DBConfig（DB_MAX_OPEN_CONNS / DB_MAX_IDLE_CONNS）控制，
// 默认 50/20：2C2G 小机原默认 20 在并发后台请求时会排队等连接。
// 连接复用时长保持常量（避免连接被 MySQL 单方面回收后拿到坏连接）。
const (
	connMaxLifetime = 30 * time.Minute
	connMaxIdleTime = 5 * time.Minute
)

// Open 建立 MySQL 连接池并做一次探活。
//
// 返回的 *sql.DB 是并发安全的，可直接被所有 handler 共享；
// 调用方负责 Close。
func Open(cfg config.DBConfig) (*sql.DB, error) {
	mc, err := mysql.ParseDSN(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("DSN 解析失败: %w", err)
	}
	// 由 ParseDSN + FormatDSN 统一负责转义：密码里带 @ : / 等字符时手写格式化会拼出坏 DSN。
	connector, err := mysql.NewConnector(mc)
	if err != nil {
		return nil, fmt.Errorf("创建连接器失败: %w", err)
	}

	db := sql.OpenDB(connector)
	maxOpen := cfg.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = 50
	}
	maxIdle := cfg.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = 20
	}
	if maxIdle > maxOpen {
		maxIdle = maxOpen
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(connMaxLifetime)
	db.SetConnMaxIdleTime(connMaxIdleTime)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("数据库不可达: %w", err)
	}

	return db, nil
}
