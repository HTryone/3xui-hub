package admin

// PaymentStore 的 database/sql 实现（支付域）。
//
// 时间口径与 P8d 的 sqlStore 完全一致（contracts/data-contract.md §4）：
// DSN 用 parseTime=true&loc=UTC，把 MySQL 返回的字面量打 UTC 标签、不做换算。
// 严禁 SET time_zone。

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// NewPaymentSQLStore 用已有连接池构造支付域的 PaymentStore。
func NewPaymentSQLStore(db *sql.DB) PaymentStore {
	return &paymentSQLStore{db: db}
}

type paymentSQLStore struct {
	db *sql.DB
}

const paymentSelectColumns = `id, name, driver_type, driver_config, gateway, query_gateway,
	member_id, api_key, notify_url, bank_code, enabled, created_at`

// paymentLegacyColumnSet 是允许写入的旧列白名单（防脏写）。
var paymentLegacyColumnSet = map[string]bool{
	"gateway": true, "query_gateway": true, "member_id": true, "api_key": true, "bank_code": true,
}

func scanPaymentRow(row interface{ Scan(...any) error }) (*PaymentRow, error) {
	var (
		p          PaymentRow
		driverCfg  sql.NullString
		queryGW    sql.NullString
		notifyURL  sql.NullString
		enabled    int
		createdAt  sql.NullTime
		driverType sql.NullString
		gateway    sql.NullString
		memberID   sql.NullString
		apiKey     sql.NullString
		bankCode   sql.NullString
	)
	if err := row.Scan(&p.ID, &p.Name, &driverType, &driverCfg, &gateway, &queryGW,
		&memberID, &apiKey, &notifyURL, &bankCode, &enabled, &createdAt); err != nil {
		return nil, err
	}
	p.DriverType = driverType.String
	p.Gateway = gateway.String
	p.MemberID = memberID.String
	p.APIKey = apiKey.String
	p.BankCode = bankCode.String
	p.QueryGateway = nullStrPtr(queryGW)
	p.NotifyURL = nullStrPtr(notifyURL)
	p.Enabled = enabled != 0
	p.CreatedAt = nullTimePtr(createdAt)
	p.DriverConfig = parseDriverConfigJSON(driverCfg.String)
	return &p, nil
}

// parseDriverConfigJSON 把 driver_config JSON 文本解析成 map[string]string。
// 非对象 / 非标量值按 PHP 的 trim((string)$v) 口径压成字符串（数字用最短往返表示）。
func parseDriverConfigJSON(raw string) map[string]string {
	out := map[string]string{}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return out
	}
	var generic map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		return out
	}
	for k, v := range generic {
		out[k] = driverConfigScalar(v)
	}
	return out
}

func driverConfigScalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "1"
		}
		return "0"
	case float64:
		// PHP (string)$float：serialize_precision=-1 的最短往返表示
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	default:
		return ""
	}
}

// PaymentsAll 对齐 PaymentConfig::orderByDesc('id')->get()。
func (s *paymentSQLStore) PaymentsAll(ctx context.Context) ([]PaymentRow, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+paymentSelectColumns+" FROM payment_configs ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PaymentRow
	for rows.Next() {
		p, err := scanPaymentRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *paymentSQLStore) PaymentByID(ctx context.Context, id uint64) (*PaymentRow, error) {
	return scanPaymentRow(s.db.QueryRowContext(ctx,
		"SELECT "+paymentSelectColumns+" FROM payment_configs WHERE id = ?", id))
}

// legacyPlaceholders 拼旧列的写入片段（只写 in.Legacy 里出现的键）。
func legacyAssignments(in PaymentWrite) (cols []string, args []any) {
	for _, col := range legacyPaymentColumns {
		if !paymentLegacyColumnSet[col] {
			continue
		}
		val, ok := in.Legacy[col]
		if !ok {
			continue
		}
		cols = append(cols, col)
		if val == nil {
			args = append(args, nil)
		} else {
			args = append(args, *val)
		}
	}
	return cols, args
}

// CreatePayment 对齐 PaymentConfig::create($data)。
func (s *paymentSQLStore) CreatePayment(ctx context.Context, in PaymentWrite) (*PaymentRow, error) {
	cfgJSON, err := json.Marshal(in.DriverConfig)
	if err != nil {
		return nil, err
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	cols := []string{"name", "driver_type", "driver_config"}
	args := []any{in.Name, in.DriverType, string(cfgJSON)}

	legacyCols, legacyArgs := legacyAssignments(in)
	cols = append(cols, legacyCols...)
	args = append(args, legacyArgs...)

	notifyURL := any(nil)
	if in.NotifyURLSet {
		if in.NotifyURL != nil {
			notifyURL = *in.NotifyURL
		}
		cols = append(cols, "notify_url")
		args = append(args, notifyURL)
	}
	if in.EnabledSet {
		enabled := 0
		if in.Enabled {
			enabled = 1
		}
		cols = append(cols, "enabled")
		args = append(args, enabled)
	}
	cols = append(cols, "created_at", "updated_at")
	args = append(args, now, now)

	marks := make([]string, len(cols))
	for i := range cols {
		marks[i] = "?"
	}
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO payment_configs ("+strings.Join(cols, ", ")+") VALUES ("+strings.Join(marks, ", ")+")",
		args...)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.PaymentByID(ctx, uint64(id))
}

// UpdatePayment 对齐 $payment->forceFill($data)->save()：
// 只写入 in 里显式出现的列（Legacy 键 / NotifyURLSet / EnabledSet）。
func (s *paymentSQLStore) UpdatePayment(ctx context.Context, id uint64, in PaymentWrite) (*PaymentRow, error) {
	cfgJSON, err := json.Marshal(in.DriverConfig)
	if err != nil {
		return nil, err
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	sets := []string{"name = ?", "driver_type = ?", "driver_config = ?", "updated_at = ?"}
	args := []any{in.Name, in.DriverType, string(cfgJSON), now}

	legacyCols, legacyArgs := legacyAssignments(in)
	for i, col := range legacyCols {
		sets = append(sets, col+" = ?")
		args = append(args, legacyArgs[i])
	}

	if in.NotifyURLSet {
		sets = append(sets, "notify_url = ?")
		if in.NotifyURL != nil {
			args = append(args, *in.NotifyURL)
		} else {
			args = append(args, nil)
		}
	}
	if in.EnabledSet {
		enabled := 0
		if in.Enabled {
			enabled = 1
		}
		sets = append(sets, "enabled = ?")
		args = append(args, enabled)
	}
	args = append(args, id)
	if _, err := s.db.ExecContext(ctx,
		"UPDATE payment_configs SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return nil, err
	}
	return s.PaymentByID(ctx, id)
}

func (s *paymentSQLStore) DeletePayment(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM payment_configs WHERE id = ?", id)
	return err
}
