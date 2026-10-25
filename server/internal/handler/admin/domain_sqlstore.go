package admin

// DomainStore 的 database/sql 实现（域名域）。
//
// 时间口径同 P8d（data-contract §4）：parseTime=true&loc=UTC。

import (
	"context"
	"database/sql"
	"time"
)

// NewDomainSQLStore 用已有连接池构造域名域的 DomainStore。
func NewDomainSQLStore(db *sql.DB) DomainStore {
	return &domainSQLStore{db: db}
}

type domainSQLStore struct {
	db *sql.DB
}

const domainSelectColumns = `id, domain, is_primary, enabled, ssl_status, cert_expires_at, last_error, created_at`

func scanDomainRow(row interface{ Scan(...any) error }) (*DomainRow, error) {
	var (
		d         DomainRow
		isPrimary int
		enabled   int
		expires   sql.NullTime
		lastErr   sql.NullString
		createdAt sql.NullTime
	)
	if err := row.Scan(&d.ID, &d.Domain, &isPrimary, &enabled, &d.SSLStatus,
		&expires, &lastErr, &createdAt); err != nil {
		return nil, err
	}
	d.IsPrimary = isPrimary != 0
	d.Enabled = enabled != 0
	d.CertExpiresAt = nullTimePtr(expires)
	d.LastError = nullStrPtr(lastErr)
	d.CreatedAt = nullTimePtr(createdAt)
	return &d, nil
}

// DomainsIndex 对齐 Domain::where('enabled',1)->orderByDesc('is_primary')->orderBy('domain')。
func (s *domainSQLStore) DomainsIndex(ctx context.Context) ([]DomainRow, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+domainSelectColumns+" FROM domains WHERE enabled = 1 "+
			"ORDER BY is_primary DESC, domain ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DomainRow
	for rows.Next() {
		d, err := scanDomainRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

func (s *domainSQLStore) DomainByID(ctx context.Context, id uint64) (*DomainRow, error) {
	return scanDomainRow(s.db.QueryRowContext(ctx,
		"SELECT "+domainSelectColumns+" FROM domains WHERE id = ?", id))
}

func (s *domainSQLStore) DomainExists(ctx context.Context, domain string) (bool, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM domains WHERE domain = ?", domain).Scan(&n)
	return n > 0, err
}

func (s *domainSQLStore) DomainEnabledCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM domains WHERE enabled = 1").Scan(&n)
	return n, err
}

// CreateDomain 对齐 Domain::create([...])（ssl_status='pending'）。
func (s *domainSQLStore) CreateDomain(ctx context.Context, domain string, isPrimary bool) (*DomainRow, error) {
	now := time.Now().UTC().Truncate(time.Second)
	primary := 0
	if isPrimary {
		primary = 1
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO domains (domain, is_primary, enabled, ssl_status, created_at, updated_at)
		 VALUES (?, ?, 1, 'pending', ?, ?)`, domain, primary, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.DomainByID(ctx, uint64(id))
}

// SetPrimary 对齐 DB::transaction：清其他人主域 + 置自己。
func (s *domainSQLStore) SetPrimary(ctx context.Context, id uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := tx.ExecContext(ctx,
		"UPDATE domains SET is_primary = 0, updated_at = ? WHERE id <> ?", now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE domains SET is_primary = 1, updated_at = ? WHERE id = ?", now, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteDomain 删除一行；若删的是主域，剩余 enabled 里字典序第一行提为主域。
// 返回被提为主域的行（无则 nil）。
func (s *domainSQLStore) DeleteDomain(ctx context.Context, id uint64) (*DomainRow, error) {
	wasPrimary := false
	{
		var isPrimary int
		err := s.db.QueryRowContext(ctx,
			"SELECT is_primary FROM domains WHERE id = ?", id).Scan(&isPrimary)
		if err != nil {
			return nil, err
		}
		wasPrimary = isPrimary != 0
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "DELETE FROM domains WHERE id = ?", id); err != nil {
		return nil, err
	}

	var next *DomainRow
	if wasPrimary {
		// 剩余 enabled 行中字典序第一行提为主域
		n, err := scanDomainRow(tx.QueryRowContext(ctx,
			"SELECT "+domainSelectColumns+" FROM domains WHERE enabled = 1 ORDER BY domain ASC LIMIT 1"))
		if err == nil {
			now := time.Now().UTC().Truncate(time.Second)
			if _, err := tx.ExecContext(ctx,
				"UPDATE domains SET is_primary = 1, updated_at = ? WHERE id = ?", now, n.ID); err != nil {
				return nil, err
			}
			n.IsPrimary = true
			next = n
		} else if err != sql.ErrNoRows {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return next, nil
}

// MarkSSLUpdating 对齐 DomainService::submit 的 apply/renew 前置。
func (s *domainSQLStore) MarkSSLUpdating(ctx context.Context, id uint64) error {
	now := time.Now().UTC().Truncate(time.Second)
	_, err := s.db.ExecContext(ctx,
		"UPDATE domains SET ssl_status = 'pending', last_error = NULL, updated_at = ? WHERE id = ?",
		now, id)
	return err
}
