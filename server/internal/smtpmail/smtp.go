// Package smtpmail 是 SMTP 发信核心（dial + TLS/STARTTLS + AUTH + DATA + MIME 构建）。
//
// api 域（验证码邮件，内部 load 配置）与 admin 域（通知/群发/测试邮件，调用方传
// SMTPSettings）共用这一份发信逻辑，避免两份 TLS/MIME 代码漂移。
package smtpmail

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
)

// Settings 是一次发信要用的 SMTP 配置（密码已解密，与 admin.SMTPSettings 同义）。
type Settings struct {
	Host       string
	Port       int
	Username   string
	Password   string
	Encryption string // "ssl" | "tls" | "none" | ""
	FromName   string
}

// DialFunc 供测试注入假连接；nil 用 net.Dialer。
type DialFunc func(ctx context.Context, network, addr string, timeout time.Duration) (net.Conn, error)

// smtpOpTimeout 是一次发信的全过程超时（含 Auth/Mail/Rcpt/Data/Quit）。
// 只覆盖建连的 10s dial 不够：net/smtp 的后续命令是阻塞调用且不设 deadline，
// SMTP 服务器挂起会把邮件队列的单 goroutine 永久卡死。
const smtpOpTimeout = 30 * time.Second

// Send 发一封 HTML 邮件（单收件人、单主题）。
//
// 与 PHP SimpleMailerService / Mail::html 语义一致：
//   - smtp_encryption 'ssl' → 隐式 TLS（465）；'tls' → STARTTLS；'none'/空 → 明文；
//   - 有用户名才做 LOGIN 认证；发件地址 = smtp_username（空则 controlhub@localhost）；
//   - InsecureSkipVerify=true（PHP 侧 stream_options.verify_peer=false 同义）。
func Send(ctx context.Context, cfg Settings, to, subject, htmlBody string, dial DialFunc) error {
	if to == "" {
		return fmt.Errorf("收件人为空")
	}
	if cfg.Host == "" {
		return fmt.Errorf("SMTP 未配置")
	}
	if dial == nil {
		dial = func(ctx context.Context, network, addr string, timeout time.Duration) (net.Conn, error) {
			d := &net.Dialer{Timeout: timeout}
			return d.DialContext(ctx, network, addr)
		}
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	conn, err := dial(ctx, "tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("连接 SMTP 失败: %w", err)
	}

	// 总超时：dial 只覆盖建连，后续 Auth/Mail/Rcpt/Data/Quit 都是阻塞调用
	// 且 net/smtp 不设 deadline —— SMTP 服务器挂起会无限阻塞，把邮件队列的
	// 单 goroutine 卡死（一封卡住，后面全发不出去）。设连接 deadline 兜底。
	deadline := time.Now().Add(smtpOpTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	// 隐式 TLS（ssl，通常 465）：先包一层再走 SMTP 协议。
	if strings.EqualFold(cfg.Encryption, "ssl") {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName:         cfg.Host,
			InsecureSkipVerify: true, // 与 PHP 侧 stream_options.verify_peer=false 一致
			MinVersion:         tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return fmt.Errorf("SMTP TLS 握手失败: %w", err)
		}
		conn = tlsConn
	}

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("SMTP 握手失败: %w", err)
	}
	defer func() { _ = client.Close() }()

	// STARTTLS（tls）：明文连接上再升级。
	if strings.EqualFold(cfg.Encryption, "tls") {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{
				ServerName:         cfg.Host,
				InsecureSkipVerify: true,
				MinVersion:         tls.VersionTLS12,
			}); err != nil {
				return fmt.Errorf("SMTP STARTTLS 失败: %w", err)
			}
		}
	}

	// 与 PHP 一致：有用户名就做 LOGIN 认证（auth_mode=login）。
	if cfg.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return fmt.Errorf("SMTP 认证失败: %w", err)
		}
	}

	from := cfg.Username
	if from == "" {
		from = "controlhub@localhost"
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("SMTP MAIL FROM 失败: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("SMTP RCPT TO 失败: %w", err)
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA 失败: %w", err)
	}
	if _, err := w.Write(BuildMessage(cfg, to, subject, htmlBody)); err != nil {
		return fmt.Errorf("写入邮件正文失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("提交邮件失败: %w", err)
	}
	return client.Quit()
}

// BuildMessage 生成一封 UTF-8 HTML 邮件（base64 正文，避免 8bit 与行长度限制）。
func BuildMessage(cfg Settings, to, subject, htmlBody string) []byte {
	var b strings.Builder
	from := cfg.Username
	if from == "" {
		from = "controlhub@localhost"
	}
	fromHeader := from
	if cfg.FromName != "" {
		fromHeader = mime.BEncoding.Encode("UTF-8", cfg.FromName) + " <" + from + ">"
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
