package main

import (
	"database/sql"
	"fmt"
)

// ensureSchema 启动时做增量建表（全部幂等 CREATE TABLE IF NOT EXISTS）。
//
// 为什么必须有这一步：install.sh 只在「全新安装」（数据库表数为 0）时执行
// server/schema/schema.sql；老用户升级（表数 > 0）直接跳过建表。于是新增的表
// 如果只写进 schema.sql，老用户升级后表根本不会建、功能一跑就报「表不存在」。
// 所以每新增一张表，都要在这里补一条幂等建表语句，保证老用户升级全自动。
func ensureSchema(db *sql.DB) error {
	stmts := []string{
		// 多 SMTP 账号池（#66）：备用发信账号，主号风控时自动轮换。
		`CREATE TABLE IF NOT EXISTS smtp_accounts (
			id bigint unsigned NOT NULL AUTO_INCREMENT,
			host varchar(255) NOT NULL,
			port int NOT NULL DEFAULT 587,
			username varchar(255) NOT NULL DEFAULT '',
			password_enc text,
			encryption varchar(8) NOT NULL DEFAULT 'tls',
			from_name varchar(100) DEFAULT NULL,
			weight int NOT NULL DEFAULT 1,
			daily_limit int NOT NULL DEFAULT 0,
			sent_today int NOT NULL DEFAULT 0,
			last_used_at timestamp NULL DEFAULT NULL,
			reset_at timestamp NULL DEFAULT NULL,
			status varchar(16) NOT NULL DEFAULT 'active',
			cooldown_until timestamp NULL DEFAULT NULL,
			error_count int NOT NULL DEFAULT 0,
			created_at timestamp NULL DEFAULT NULL,
			updated_at timestamp NULL DEFAULT NULL,
			PRIMARY KEY (id),
			KEY smtp_accounts_status_index (status)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("增量建表失败: %w", err)
		}
	}
	return nil
}
