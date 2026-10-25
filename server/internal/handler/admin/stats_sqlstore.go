package admin

// StatsStore 的 database/sql 实现（统计域）。
//
// 只统计 status='paid'，金额取 orders.amount（实付，已扣优惠码）。

import (
	"context"
	"database/sql"
	"time"
)

// NewStatsSQLStore 用已有连接池构造统计域的 StatsStore。
func NewStatsSQLStore(db *sql.DB) StatsStore {
	return &statsSQLStore{db: db}
}

type statsSQLStore struct {
	db *sql.DB
}

func (s *statsSQLStore) RevenueToday(ctx context.Context, start *time.Time) (string, int64, error) {
	q := `SELECT COALESCE(SUM(amount), 0), COUNT(*) FROM orders WHERE status = 'paid' AND paid_at >= ?`
	var (
		amount sql.NullString
		orders int64
	)
	if err := s.db.QueryRowContext(ctx, q, nullTimeValue(start)).Scan(&amount, &orders); err != nil {
		return "", 0, err
	}
	if !amount.Valid {
		return "0.00", orders, nil
	}
	return amount.String, orders, nil
}

func (s *statsSQLStore) RevenueTotal(ctx context.Context) (string, int64, error) {
	q := `SELECT COALESCE(SUM(amount), 0), COUNT(*) FROM orders WHERE status = 'paid'`
	var (
		amount sql.NullString
		orders int64
	)
	if err := s.db.QueryRowContext(ctx, q).Scan(&amount, &orders); err != nil {
		return "", 0, err
	}
	if !amount.Valid {
		return "0.00", orders, nil
	}
	return amount.String, orders, nil
}
