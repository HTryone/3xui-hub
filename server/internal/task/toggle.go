package task

// toggle.go 是定时任务的「统一开关」读取层。
//
// 设计（用户 2026-11-08 定，要求最小改动、可扩展）：
//   - 每个调度任务都可以在 site_configs 里放一个 `sched_<任务名>_enabled` 键来关掉它；
//   - **键不存在 = 开启**（默认开），所以老库不用做任何迁移，行为与改造前一致；
//   - 值为 '0'/'false'/'off'/'no'（忽略大小写）才算关闭，其余（含空）都按开启；
//   - 关掉的效果是「整条任务不跑」，并且系统状态页也不显示它（页面侧用同一个键判断）。
//
// 为什么放 site_configs 而不是加一张表：后台改一个键即可，不用迁移、不用重启；
// 以后要给别的任务加开关，只要让那个任务名进来、写对应的键，一行代码都不用改。

import (
	"context"
	"database/sql"
	"strings"
)

// TaskToggleKey 返回某条调度任务的开关配置键。
func TaskToggleKey(name string) string {
	return "sched_" + name + "_enabled"
}

// ToggleFunc 判断某条调度任务当前是否启用（返回 false = 整条任务关闭）。
type ToggleFunc func(ctx context.Context, name string) (bool, error)

// sqlToggle 是 ToggleFunc 的 MySQL 实现（读 site_configs，缺省开）。
type sqlToggle struct {
	db *sql.DB
}

// NewSQLToggle 用已有连接池构造开关读取器。
func NewSQLToggle(db *sql.DB) ToggleFunc {
	t := &sqlToggle{db: db}
	return t.enabled
}

func (t *sqlToggle) enabled(ctx context.Context, name string) (bool, error) {
	var v sql.NullString
	err := t.db.QueryRowContext(ctx,
		"SELECT value FROM site_configs WHERE `key` = ?", TaskToggleKey(name)).Scan(&v)
	if err == sql.ErrNoRows {
		return true, nil // 没配过 = 开启（默认开，兼容老库）
	}
	if err != nil {
		return true, err // 读配置出错不阻断任务，按开启处理
	}
	return !isOffValue(v.String), nil
}

// isOffValue 判断配置值是否表示「关闭」。
func isOffValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "off", "no":
		return true
	default:
		return false
	}
}
