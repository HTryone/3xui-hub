package main

import (
	"database/sql"
	"fmt"
)

// ensureSchema 启动时做增量建表 / 加列（全部幂等，老用户升级不用手跑 SQL）。
//
// 为什么必须有这一步：install.sh 只在「全新安装」（数据库表数为 0）时执行
// server/schema/schema.sql；老用户升级（表数 > 0）直接跳过建表。于是新增的表
// 如果只写进 schema.sql，老用户升级后表根本不会建、功能一跑就报「表不存在」。
// 新增列同理：老库的 xray_inbounds 不会有新列，读写会直接 Unknown column 报错。
// 所以每新增一张表 / 一列，都要在这里补一条幂等语句，保证老用户升级全自动。
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
		// TLS 证书库（HY2）：入站只存 tlsCertId，私钥加密后放这里，
		// 渲染配置时才解密注入 tlsSettings.certificates（私钥永不进 xray_inbounds.stream_settings）。
		`CREATE TABLE IF NOT EXISTS xray_certificates (
			id bigint unsigned NOT NULL AUTO_INCREMENT,
			name varchar(190) NOT NULL,
			domain varchar(190) DEFAULT NULL,
			cert_pem mediumtext,
			key_enc mediumtext,
			not_after datetime DEFAULT NULL,
			created_at timestamp NULL DEFAULT NULL,
			updated_at timestamp NULL DEFAULT NULL,
			PRIMARY KEY (id),
			UNIQUE KEY xray_certificates_name_unique (name)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("增量建表失败: %w", err)
		}
	}

	// 增量加列：HY2 的 salamander 混淆密码（issue #94）。密码是高敏字段，
	// 独立加密列存密文，stream_settings 里只留 {"type":"salamander"} 占位。
	columns := []struct{ table, column, definition string }{
		{"xray_inbounds", "salamander_password", "text COLLATE utf8mb4_unicode_ci"},
	}
	for _, c := range columns {
		if err := ensureColumn(db, c.table, c.column, c.definition); err != nil {
			return err
		}
	}
	return nil
}

// ensureColumn 幂等加列。
//
// MySQL 没有 `ADD COLUMN IF NOT EXISTS`（MariaDB 才有），所以先查
// information_schema 再决定要不要 ALTER —— 重复启动、重复升级都不会报
// 「Duplicate column name」。
func ensureColumn(db *sql.DB, table, column, definition string) error {
	var n int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM information_schema.COLUMNS "+
			"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?",
		table, column).Scan(&n)
	if err != nil {
		return fmt.Errorf("检查列 %s.%s 失败: %w", table, column, err)
	}
	if n > 0 {
		return nil
	}
	if _, err := db.Exec("ALTER TABLE `" + table + "` ADD COLUMN `" + column + "` " + definition); err != nil {
		return fmt.Errorf("增量加列 %s.%s 失败: %w", table, column, err)
	}
	return nil
}
