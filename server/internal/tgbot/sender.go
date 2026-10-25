package tgbot

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"controlhub-server/internal/crypto"
)

// 配置 key（site_configs，与 admin 域 telegram 配置页共用同一组）。
const (
	KeyToken         = "tg_bot_token"          // 加密存储
	KeyChatID        = "tg_bot_chat_id"        // 接收方 chat_id（数字）
	KeyEnabled       = "tg_enabled"            // 总开关 "1"/"0"
	KeyNotifyNodeOff = "tg_notify_node_offline" // 节点离线告警
	KeyNotifyPayment = "tg_notify_payment"      // 支付到账通知
	KeyBackupPush    = "tg_backup_push"         // 每日备份推送
)

// Sender 是「读面板 TG 配置 → 解密 token → 发消息」的封装。
// 供 collector / task 等非 admin 包在告警触发点调用，不依赖 handler 层。
type Sender struct {
	db     *sql.DB
	appKey []byte
	logf   func(format string, args ...any)
}

// NewSender 构造 Sender。logf nil 则静默。
func NewSender(db *sql.DB, appKey []byte, logf func(format string, args ...any)) *Sender {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Sender{db: db, appKey: appKey, logf: logf}
}

// Send 发一条文本通知：读配置，若未启用/未配置则静默跳过（不报错、不打扰）。
func (s *Sender) Send(ctx context.Context, text string) error {
	c, err := s.load(ctx)
	if err != nil || c == nil {
		return err // 未配置/关闭返回 nil，读配置出错返回 err
	}
	client := NewClient(c.token, nil)
	if err := client.SendMessage(ctx, c.chatID, text); err != nil {
		return fmt.Errorf("TG 发消息失败: %w", err)
	}
	return nil
}

// SendDocument 发文件（备份推送用）。同样读配置、未启用静默跳过。
func (s *Sender) SendDocument(ctx context.Context, caption, filePath string) error {
	c, err := s.load(ctx)
	if err != nil || c == nil {
		return err
	}
	client := NewClient(c.token, nil)
	if err := client.SendDocument(ctx, c.chatID, caption, filePath); err != nil {
		return fmt.Errorf("TG 发文件失败: %w", err)
	}
	return nil
}

// NotifyEnabled 读某个告警开关（节点离线/支付/备份）。供触发点先判断再决定是否发。
func (s *Sender) NotifyEnabled(ctx context.Context, key string) bool {
	var v sql.NullString
	if err := s.db.QueryRowContext(ctx,
		"SELECT value FROM site_configs WHERE `key` = ?", key).Scan(&v); err != nil {
		return false
	}
	return v.Valid && v.String == "1"
}

// Notify 按场景开关发文本消息：开关没开则静默跳过。
func (s *Sender) Notify(ctx context.Context, switchKey, text string) error {
	if !s.NotifyEnabled(ctx, switchKey) {
		return nil
	}
	return s.Send(ctx, text)
}

// tgConfig 是解密后的可发送配置。
type tgConfig struct {
	token  string
	chatID int64
}

// load 读配置并解密 token；未启用或未配置返回 (nil, nil)（静默跳过）。
func (s *Sender) load(ctx context.Context) (*tgConfig, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT `key`, value FROM site_configs WHERE `key` IN (?, ?, ?)",
		KeyToken, KeyChatID, KeyEnabled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var token, chatIDStr, enabled string
	for rows.Next() {
		var k, v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		switch k.String {
		case KeyToken:
			token = v.String
		case KeyChatID:
			chatIDStr = v.String
		case KeyEnabled:
			enabled = v.String
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if enabled != "1" || token == "" || chatIDStr == "" {
		return nil, nil // 未启用 / 未配置 → 静默
	}

	plain, err := crypto.DecryptString(token, s.appKey)
	if err != nil {
		s.logf("TG token 解密失败: %v", err)
		return nil, nil // 解密失败按未配置处理，不反复报错
	}
	chatID, err := strconv.ParseInt(strings.TrimSpace(chatIDStr), 10, 64)
	if err != nil || chatID <= 0 {
		s.logf("TG chat_id 非法: %q", chatIDStr)
		return nil, nil
	}
	return &tgConfig{token: plain, chatID: chatID}, nil
}
