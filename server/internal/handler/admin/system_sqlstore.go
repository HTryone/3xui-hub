package admin

// SystemStore 的 database/sql 实现（系统域）。
//
// 时间口径同 P8d（data-contract §4）：parseTime=true&loc=UTC。

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// NewSystemSQLStore 用已有连接池构造系统域的 SystemStore。
func NewSystemSQLStore(db *sql.DB) SystemStore {
	return &systemSQLStore{db: db}
}

type systemSQLStore struct {
	db *sql.DB
}

// OperationLogsRecent 对齐 status() 的最近 N 条（按 id 倒序）。
func (s *systemSQLStore) OperationLogsRecent(ctx context.Context, limit int64) ([]OperationLogRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, type, actor_name, action, method, path, status, error, created_at
		 FROM operation_logs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OperationLogRow
	for rows.Next() {
		var (
			l         OperationLogRow
			actorName sql.NullString
			method    sql.NullString
			path      sql.NullString
			status    sql.NullInt64
			errText   sql.NullString
			createdAt sql.NullTime
		)
		if err := rows.Scan(&l.ID, &l.Type, &actorName, &l.Action, &method, &path,
			&status, &errText, &createdAt); err != nil {
			return nil, err
		}
		l.ActorName = nullStrPtr(actorName)
		l.Method = nullStrPtr(method)
		l.Path = nullStrPtr(path)
		if status.Valid {
			n := status.Int64
			l.Status = &n
		}
		l.Error = nullStrPtr(errText)
		l.CreatedAt = nullTimePtr(createdAt)
		out = append(out, l)
	}
	return out, rows.Err()
}

// OperationLogPage 对齐 auditLogs 的筛选 + 分页。
func (s *systemSQLStore) OperationLogPage(ctx context.Context, f AuditLogFilter, page, perPage int64) ([]OperationLogRow, int64, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.Type != "" {
		where = append(where, "type = ?")
		args = append(args, f.Type)
	}
	switch f.Status {
	case "error":
		where = append(where, "status >= 400")
	case "ok":
		where = append(where, "status < 400")
	}
	if f.Search != "" {
		like := "%" + f.Search + "%"
		where = append(where, "(action LIKE ? OR path LIKE ? OR actor_name LIKE ? OR error LIKE ?)")
		args = append(args, like, like, like, like)
	}
	whereSQL := " WHERE " + strings.Join(where, " AND ")

	var total int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM operation_logs"+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if perPage < 1 {
		perPage = 50
	}
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * perPage

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, type, actor_name, action, method, path, status, error, ip, created_at
		 FROM operation_logs`+whereSQL+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(append([]any{}, args...), perPage, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []OperationLogRow
	for rows.Next() {
		var (
			l         OperationLogRow
			actorName sql.NullString
			method    sql.NullString
			path      sql.NullString
			status    sql.NullInt64
			errText   sql.NullString
			ip        sql.NullString
			createdAt sql.NullTime
		)
		if err := rows.Scan(&l.ID, &l.Type, &actorName, &l.Action, &method, &path,
			&status, &errText, &ip, &createdAt); err != nil {
			return nil, 0, err
		}
		l.ActorName = nullStrPtr(actorName)
		l.Method = nullStrPtr(method)
		l.Path = nullStrPtr(path)
		if status.Valid {
			n := status.Int64
			l.Status = &n
		}
		l.Error = nullStrPtr(errText)
		l.IP = nullStrPtr(ip)
		l.CreatedAt = nullTimePtr(createdAt)
		out = append(out, l)
	}
	return out, total, rows.Err()
}

func scanScheduledRun(row interface{ Scan(...any) error }) (*ScheduledTaskRun, error) {
	var (
		r      ScheduledTaskRun
		errT   sql.NullString
		dur    sql.NullInt64
		ranAt  sql.NullTime
	)
	if err := row.Scan(&r.Command, &r.Status, &errT, &dur, &ranAt); err != nil {
		return nil, err
	}
	r.Error = nullStrPtr(errT)
	if dur.Valid {
		n := dur.Int64
		r.DurationMS = &n
	}
	r.RanAt = nullTimePtr(ranAt)
	return &r, nil
}

// ScheduledTaskRunLast 对齐 scheduled_task_runs 的最近一次运行。
func (s *systemSQLStore) ScheduledTaskRunLast(ctx context.Context, command string) (*ScheduledTaskRun, error) {
	r, err := scanScheduledRun(s.db.QueryRowContext(ctx,
		`SELECT command, status, error, duration_ms, ran_at
		 FROM scheduled_task_runs WHERE command = ? ORDER BY id DESC LIMIT 1`, command))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// ScheduledTaskRunsRecent 对齐最近 N 条运行记录。
func (s *systemSQLStore) ScheduledTaskRunsRecent(ctx context.Context, limit int64) ([]ScheduledTaskRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT command, status, error, duration_ms, ran_at
		 FROM scheduled_task_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduledTaskRun
	for rows.Next() {
		r, err := scanScheduledRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// WorkerHeartbeat 对齐 Cache::get('controlhub:worker-heartbeat')。
// database cache store：cache 表的 key/value（value 是序列化的时间串）。
func (s *systemSQLStore) WorkerHeartbeat(ctx context.Context) (*time.Time, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT value FROM cache WHERE `key` = ?", "controlhub:worker-heartbeat").Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	// Laravel database cache 的值可能是序列化串或 ISO 时间串，都试一遍
	val := strings.TrimSpace(raw.String)
	val = strings.Trim(val, `"`)
	if t, err := time.Parse(time.RFC3339, val); err == nil {
		return &t, nil
	}
	if t, err := time.Parse("2006-01-02 15:04:05", val); err == nil {
		u := t.UTC()
		return &u, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, val); err == nil {
		return &t, nil
	}
	return nil, nil
}

// QueueCounts 返回 jobs 表 pending / running 与 failed_jobs 总数。
func (s *systemSQLStore) QueueCounts(ctx context.Context) (int64, int64, int64, error) {
	var pending, running, failed int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM jobs WHERE reserved_at IS NULL").Scan(&pending); err != nil {
		return 0, 0, 0, err
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM jobs WHERE reserved_at IS NOT NULL").Scan(&running); err != nil {
		return 0, 0, 0, err
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM failed_jobs").Scan(&failed); err != nil {
		return 0, 0, 0, err
	}
	return pending, running, failed, nil
}

// FailedJobsRecent 对齐 failed_jobs 最近 N 条（任务名从 payload 的 displayName 取）。
func (s *systemSQLStore) FailedJobsRecent(ctx context.Context, limit int64) ([]SystemFailedJob, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT queue, payload, exception, failed_at
		 FROM failed_jobs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SystemFailedJob
	for rows.Next() {
		var (
			f        SystemFailedJob
			queue    sql.NullString
			payload  sql.NullString
			except   sql.NullString
			failedAt sql.NullTime
		)
		if err := rows.Scan(&queue, &payload, &except, &failedAt); err != nil {
			return nil, err
		}
		f.Queue = queue.String
		f.Name = jobNameFromPayload(payload.String)
		f.ErrorSummary = errorSummary(except.String)
		f.FailedAt = nullTimePtr(failedAt)
		// failed_jobs 无 attempts 列，拿不到就 null，不伪造 0
		out = append(out, f)
	}
	return out, rows.Err()
}
