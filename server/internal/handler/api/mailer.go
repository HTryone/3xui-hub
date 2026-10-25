package api

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"controlhub-server/internal/config"
	"controlhub-server/internal/crypto"
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
	dial func(ctx context.Context, network, addr string, timeout time.Duration) (net.Conn, error)
}

// NewSMTPMailer 构造 SMTP 发信器。cfg 用于取 APP_KEY（解密 smtp_password）。
func NewSMTPMailer(store Store, cfg *config.Config, logger interface{ Printf(string, ...any) }) Mailer {
	m := &SMTPMailer{store: store, logger: logger}
	if cfg != nil {
		m.appKey = cfg.AppKey
	}
	m.dial = func(ctx context.Context, network, addr string, timeout time.Duration) (net.Conn, error) {
		d := &net.Dialer{Timeout: timeout}
		return d.DialContext(ctx, network, addr)
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
	if to == "" {
		return fmt.Errorf("收件人为空")
	}
	s, err := m.load(ctx)
	if err != nil {
		return fmt.Errorf("读取 SMTP 配置失败: %w", err)
	}
	if s.Host == "" {
		return fmt.Errorf("SMTP 未配置")
	}

	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	conn, err := m.dial(ctx, "tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("连接 SMTP 失败: %w", err)
	}

	// 隐式 TLS（ssl，通常 465）：先包一层再走 SMTP 协议。
	if strings.EqualFold(s.Encryption, "ssl") {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName:         s.Host,
			InsecureSkipVerify: true, // 与 PHP 侧 stream_options.verify_peer=false 一致
			MinVersion:         tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return fmt.Errorf("SMTP TLS 握手失败: %w", err)
		}
		conn = tlsConn
	}

	client, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("SMTP 握手失败: %w", err)
	}
	defer func() { _ = client.Close() }()

	// STARTTLS（tls）：明文连接上再升级。
	if strings.EqualFold(s.Encryption, "tls") {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{
				ServerName:         s.Host,
				InsecureSkipVerify: true,
				MinVersion:         tls.VersionTLS12,
			}); err != nil {
				return fmt.Errorf("SMTP STARTTLS 失败: %w", err)
			}
		}
	}

	// 与 PHP 一致：有用户名就做 LOGIN 认证（auth_mode=login）。
	if s.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return fmt.Errorf("SMTP 认证失败: %w", err)
		}
	}

	if from := s.Username; from != "" {
		if err := client.Mail(from); err != nil {
			return fmt.Errorf("SMTP MAIL FROM 失败: %w", err)
		}
	} else if err := client.Mail("controlhub@localhost"); err != nil {
		return fmt.Errorf("SMTP MAIL FROM 失败: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("SMTP RCPT TO 失败: %w", err)
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA 失败: %w", err)
	}
	if _, err := w.Write(buildMessage(s, to, subject, htmlBody)); err != nil {
		return fmt.Errorf("写入邮件正文失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("提交邮件失败: %w", err)
	}
	return client.Quit()
}

// buildMessage 生成一封 UTF-8 HTML 邮件（base64 正文，避免 8bit 与行长度限制）。
func buildMessage(s smtpSettings, to, subject, htmlBody string) []byte {
	var b strings.Builder
	from := s.Username
	if from == "" {
		from = "controlhub@localhost"
	}
	fromHeader := from
	if s.FromName != "" {
		fromHeader = mime.BEncoding.Encode("UTF-8", s.FromName) + " <" + from + ">"
	}

	b.WriteString("From: " + fromHeader + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + mime.BEncoding.Encode("UTF-8", subject) + "\r\n")
	b.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	b.WriteString("Message-ID: <" + newMessageID() + ">\r\n")
	b.WriteString("\r\n")
	b.WriteString(wrapBase64(base64.StdEncoding.EncodeToString([]byte(htmlBody))))
	return []byte(b.String())
}

// wrapBase64 按 76 列折行（RFC 2045）。
func wrapBase64(enc string) string {
	var b strings.Builder
	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteString("\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc)
	b.WriteString("\r\n")
	return b.String()
}

// newMessageID 生成一个足够唯一的 Message-ID 本地部分。
func newMessageID() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("%x@controlhub", buf)
}
