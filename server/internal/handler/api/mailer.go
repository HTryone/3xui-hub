package api

import (
	"context"
	"fmt"
	"strconv"

	"controlhub-server/internal/config"
	"controlhub-server/internal/crypto"
	"controlhub-server/internal/smtpmail"
)

// SMTPMailer 是 SimpleMailerService 的 Go 复刻：
//
//   - 每次发信都从 site_configs 现读 SMTP 配置（后台改完立即生效，不缓存）；
//   - 发件地址 = smtp_username（两者必须一致，与 PHP 的类注释一致）；
//   - smtp_password 是 Crypt::encryptString 密文，解密失败按空密码继续（只记日志）；
//   - smtp_encryption：'none' → 明文；'ssl' → 隐式 TLS（465）；'tls' → STARTTLS。
//
// 与 PHP 的差异：PHP 走 Laravel Mail + Symfony Mailer（有连接复用与更全的 MIME 能力），
// 这里只发「纯 HTML、单收件人」的验证码邮件 —— 足够覆盖 P2 的两个发码端点。
type SMTPMailer struct {
	store  Store
	appKey []byte
	logger interface{ Printf(string, ...any) }
	// dial 供测试注入假连接（默认 net.Dialer）。
	dial smtpmail.DialFunc
}

// NewSMTPMailer 构造 SMTP 发信器。cfg 用于取 APP_KEY（解密 smtp_password）。
func NewSMTPMailer(store Store, cfg *config.Config, logger interface{ Printf(string, ...any) }) Mailer {
	m := &SMTPMailer{store: store, logger: logger}
	if cfg != nil {
		m.appKey = cfg.AppKey
	}
	return m
}

// smtpSettings 是一次发信用的配置快照。
type smtpSettings struct {
	Host       string
	Port       int
	Username   string
	Password   string
	Encryption string // "ssl" | "tls" | "none" | ""
	FromName   string
}

// load 读取并解密 SMTP 配置。
func (m *SMTPMailer) load(ctx context.Context) (smtpSettings, error) {
	values, err := m.store.SiteConfigMany(ctx, smtpConfigKeys)
	if err != nil {
		return smtpSettings{}, err
	}

	s := smtpSettings{
		Host:       values["smtp_host"],
		Username:   values["smtp_username"],
		Encryption: values["smtp_encryption"],
		FromName:   values["smtp_from_name"],
	}
	if rawPort := values["smtp_port"]; rawPort != "" {
		p, convErr := strconv.Atoi(rawPort)
		if convErr != nil || p <= 0 {
			p = 587
		}
		s.Port = p
	} else {
		s.Port = 587
	}

	// 解密密码：失败只记日志、按空密码继续（与原实现一致）。
	if enc := values["smtp_password"]; enc != "" {
		plain, decErr := crypto.DecryptString(enc, m.appKey)
		if decErr != nil {
			if m.logger != nil {
				m.logger.Printf("SMTP password decrypt failed: %v", decErr)
			}
		} else {
			s.Password = plain
		}
	}
	return s, nil
}

// Send 发一封 HTML 邮件（单收件人、单主题）。
func (m *SMTPMailer) Send(ctx context.Context, to, subject, htmlBody string) error {
	s, err := m.load(ctx)
	if err != nil {
		return fmt.Errorf("读取 SMTP 配置失败: %w", err)
	}
	return smtpmail.Send(ctx, smtpmail.Settings{
		Host:       s.Host,
		Port:       s.Port,
		Username:   s.Username,
		Password:   s.Password,
		Encryption: s.Encryption,
		FromName:   s.FromName,
	}, to, subject, htmlBody, m.dial)
}
