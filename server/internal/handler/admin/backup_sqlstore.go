package admin

// BackupStore 的 database/sql 实现（备份域）。
//
// 表名来自固定白名单（backupDiffTables），不在 SQL 里拼用户输入。

import (
	"context"
	"database/sql"
	"strings"
)

// NewBackupSQLStore 用已有连接池构造备份域的 BackupStore。
func NewBackupSQLStore(db *sql.DB) BackupStore {
	return &backupSQLStore{db: db}
}

type backupSQLStore struct {
	db *sql.DB
}

// backupMergeTables 是 merge 模式要合并的表与去重键（对齐 mergeImport）。
var backupMergeTables = []struct {
	Table string
	// On 是 insert-if-absent 的判重列组合
	On []string
}{
	{"plans", []string{"name"}},
	{"users", []string{"email"}},
	{"nodes", []string{"host", "port"}},
	{"payment_configs", []string{"name"}},
	{"node_inbounds", []string{"node_id", "protocol", "inbound_id"}},
	{"orders", []string{"order_no"}},
}

// TableCounts 对齐 calculateDiffFromSql 里 DB::table($table)->count()。
func (s *backupSQLStore) TableCounts(ctx context.Context, tables []string) (map[string]int64, error) {
	out := make(map[string]int64, len(tables))
	for _, table := range tables {
		// 表名来自固定白名单，仍按标识符过滤防脏
		if !isSafeSQLIdent(table) {
			out[table] = 0
			continue
		}
		var n int64
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"`").Scan(&n); err != nil {
			return nil, err
		}
		out[table] = n
	}
	return out, nil
}

// MergeFromDatabase 对齐 mergeImport 的事务内逐表 insert-if-absent。
func (s *backupSQLStore) MergeFromDatabase(ctx context.Context, sourceDB string) error {
	if !isSafeSQLIdent(sourceDB) {
		return errUnsafeIdent
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, spec := range backupMergeTables {
		if !isSafeSQLIdent(spec.Table) {
			continue
		}
		rows, err := tx.QueryContext(ctx, "SELECT * FROM `"+sourceDB+"`.`"+spec.Table+"`")
		if err != nil {
			return err
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return err
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return err
			}
			// 判重
			where := make([]string, 0, len(spec.On))
			args := make([]any, 0, len(spec.On))
			for _, onCol := range spec.On {
				idx := -1
				for i, c := range cols {
					if c == onCol {
						idx = i
						break
					}
				}
				if idx < 0 {
					continue
				}
				where = append(where, "`"+onCol+"` = ?")
				args = append(args, normalizeSQLValue(vals[idx]))
			}
			if len(where) == 0 {
				continue
			}
			var exists int64
			q := "SELECT COUNT(*) FROM `" + spec.Table + "` WHERE " + strings.Join(where, " AND ")
			if err := tx.QueryRowContext(ctx, q, args...).Scan(&exists); err != nil {
				rows.Close()
				return err
			}
			if exists > 0 {
				continue
			}
			// 插入
			marks := make([]string, len(cols))
			insertArgs := make([]any, len(cols))
			colSQL := make([]string, len(cols))
			for i, c := range cols {
				if !isSafeSQLIdent(c) {
					marks[i] = "NULL"
					continue
				}
				colSQL[i] = "`" + c + "`"
				marks[i] = "?"
				insertArgs[i] = normalizeSQLValue(vals[i])
			}
			// 列名与占位符一一对应
			finalCols := []string{}
			finalMarks := []string{}
			finalArgs := []any{}
			for i := range cols {
				if marks[i] == "NULL" && colSQL[i] == "" {
					continue
				}
				finalCols = append(finalCols, colSQL[i])
				finalMarks = append(finalMarks, marks[i])
				if marks[i] == "?" {
					finalArgs = append(finalArgs, insertArgs[i])
				}
			}
			_, err := tx.ExecContext(ctx,
				"INSERT INTO `"+spec.Table+"` ("+strings.Join(finalCols, ", ")+") VALUES ("+
					strings.Join(finalMarks, ", ")+")",
				finalArgs...)
			if err != nil {
				rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return tx.Commit()
}

// isSafeSQLIdent 只允许 [A-Za-z0-9_]。
func isSafeSQLIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '_' {
			continue
		}
		return false
	}
	return true
}

// errUnsafeIdent 标识符不合法。
var errUnsafeIdent = errUnsafe("backup: 不合法的 SQL 标识符")

type errUnsafe string

func (e errUnsafe) Error() string { return string(e) }

// normalizeSQLValue 把 database/sql 的 []byte 归一成 string（MySQL 文本列）。
func normalizeSQLValue(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	default:
		return v
	}
}
