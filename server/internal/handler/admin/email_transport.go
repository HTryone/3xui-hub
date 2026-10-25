package admin

import (
	"context"

	"controlhub-server/internal/smtpmail"
)

// smtpMailTransport 是 MailTransport 的真实 SMTP 实现（对齐 SimpleMailerService::send）。
//
// 每次 Send 都由调用方把已解密的 SMTPSettings 传进来，不在 Transport 内缓存连接配置
// —— 改 mail 配置后下一封信立即生效（Go 侧 forgetMailers 的等价物）。
type smtpMailTransport struct {
	dial   smtpmail.DialFunc
	logger loggerLike
}

// NewSMTPTransport 构造真实 SMTP 发信器。
func NewSMTPTransport(logger loggerLike) MailTransport {
	return &smtpMailTransport{logger: logger}
}

// Send 发一封 HTML 邮件（发信核心在 internal/smtpmail，与 api 域共用）。
func (t *smtpMailTransport) Send(cfg SMTPSettings, to, subject, htmlBody string) error {
	return smtpmail.Send(context.Background(), smtpmail.Settings{
		Host:       cfg.Host,
		Port:       cfg.Port,
		Username:   cfg.Username,
		Password:   cfg.Password,
		Encryption: cfg.Encryption,
		FromName:   cfg.FromName,
	}, to, subject, htmlBody, t.dial)
}
