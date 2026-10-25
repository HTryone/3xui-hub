package ws

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// SqOutboundStore 是 OutboundStore 的 MySQL 实现（xray_outbound_traffic 表）。
//
// 对齐 NodeWsServer::storeOutboundTraffic()：
//   - 表未建时静默跳过（PHP Schema::hasTable）；
//   - 按 (node_id, tag) upsert 覆盖写 upload/download/updated_at（无历史）；
//   - tag 截断 mb_substr(0, 128)（调用方已截，这里再守一道）。
type SqOutboundStore struct {
	DB  *sql.DB
	Now func() time.Time // nil → time.Now().UTC()

	// tableCache 记住「表是否存在」的探测结果，避免每条 report 都查 information_schema。
	// 表是 Laravel migration 建的，运行期不会中途出现/消失；即便「建表后第一轮跳过」，
	// 也无非丢一条覆盖写，下一轮就有表了。探测出错不入缓存，下次重试。
	tableMu      sync.Mutex
	tableChecked bool
	tableExists  bool
}

func (s *SqOutboundStore) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// hasTable 探测 xray_outbound_traffic 是否已建（PHP Schema::hasTable）。
func (s *SqOutboundStore) hasTable(ctx context.Context) (bool, error) {
	s.tableMu.Lock()
	if s.tableChecked {
		ok := s.tableExists
		s.tableMu.Unlock()
		return ok, nil
	}
	s.tableMu.Unlock()

	var n int
	if err := s.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?",
		"xray_outbound_traffic").Scan(&n); err != nil {
		return false, fmt.Errorf("探测 xray_outbound_traffic 表失败: %w", err)
	}
	ok := n > 0
	s.tableMu.Lock()
	s.tableChecked, s.tableExists = true, ok
	s.tableMu.Unlock()
	return ok, nil
}

// StoreOutbound 复刻 storeOutboundTraffic() 的 upsert。
func (s *SqOutboundStore) StoreOutbound(ctx context.Context, nodeID int64, rows []OutboundRow) error {
	if len(rows) == 0 {
		return nil
	}
	ok, err := s.hasTable(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil // 表未建：静默跳过（S4 出站管理未启用时的常态）
	}

	now := s.now().Format("2006-01-02 15:04:05")
	for _, r := range rows {
		tag := mbSubstr(r.Tag, 128)
		if tag == "" {
			continue
		}
		// 与 traffic_snapshots 同口径：唯一键 (node_id, tag) 覆盖写，不存历史。
		_, err := s.DB.ExecContext(ctx,
			"INSERT INTO xray_outbound_traffic (node_id, tag, upload, download, updated_at) VALUES (?, ?, ?, ?, ?) "+
				"ON DUPLICATE KEY UPDATE upload = VALUES(upload), download = VALUES(download), updated_at = VALUES(updated_at)",
			nodeID, tag, r.Upload, r.Download, now)
		if err != nil {
			return fmt.Errorf("写入出站流量（node=%d tag=%s）失败: %w", nodeID, tag, err)
		}
	}
	return nil
}

// NopOutboundStore 是「什么都不做」的 OutboundStore（测试 / 出站功能未启用时）。
type NopOutboundStore struct{}

// StoreOutbound 直接返回 nil。
func (NopOutboundStore) StoreOutbound(context.Context, int64, []OutboundRow) error { return nil }
