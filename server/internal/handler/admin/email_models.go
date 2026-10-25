package admin

// 管理端邮件域（P8c）的类型与依赖。
//
// 逐条对齐 backend/app/Http/Controllers/Admin/EmailController.php 与
// backend/app/Services/{MailNotifyService,MailScheduleService,MailBatchService,SimpleMailerService}.php。

import (
	"context"
	"log"
	"time"
)

// ---- SiteConfig 键（EmailController::KEYS + MailNotifyService::CONFIG_KEYS + MailScheduleService）----

// emailConfigKeys 对齐 EmailController::KEYS（show/save 的完整键集，保序）。
var emailConfigKeys = []string{
	"smtp_host",
	"smtp_port",
	"smtp_username",
	"smtp_password", // 加密存储
	"smtp_encryption",
	"smtp_from_name",
	"email_template",
	"register_email_verify",
	"mail_rate_limit_enabled",
	"mail_rate_per_minute",
	"rate_code_interval",
	"rate_code_per_day",
	"rate_code_ip_hourly",
	"rate_verify_max_attempts",
	"rate_verify_lock_seconds",
	"rate_login_max_attempts",
	"rate_login_lock_seconds",
	"rate_register_ip_hourly",
}

// mailNotifyScenes 对齐 MailNotifyService::SCENES（保序）。
var mailNotifyScenes = []string{"traffic_exhausted", "traffic_almost", "expiring", "expired"}

// mailNotifySceneDefaults 是每个场景的出厂标题/正文/收件人。
var mailNotifySceneDefaults = map[string]struct {
	Label string
	Title string
	Body  string
	To    string
}{
	"traffic_exhausted": {
		Label: "流量用尽",
		Title: "您的流量已用尽",
		Body:  "<p>您好，</p><p>您的流量已用尽，流量已被暂停。请续费或重置流量后继续使用。</p>",
		To:    "user",
	},
	"traffic_almost": {
		Label: "流量即将用尽",
		Title: "您的流量即将用尽",
		Body:  "<p>您好，</p><p>您的流量已使用 {{percent}}%，请及时续费，避免服务中断。</p>",
		To:    "user",
	},
	"expiring": {
		Label: "套餐即将到期",
		Title: "您的套餐即将到期",
		Body:  "<p>您好，</p><p>您的套餐将于 {{expire_date}} 到期（剩余 {{days_left}} 天），请及时续费。</p>",
		To:    "user",
	},
	"expired": {
		Label: "套餐已到期",
		Title: "您的套餐已到期",
		Body:  "<p>您好，</p><p>您的套餐已于 {{expire_date}} 到期，流量已关闭。续费后可继续使用。</p>",
		To:    "user",
	},
}

// MailLog type 常量（对齐 MailLog 模型）。
const (
	mailLogTypeNotify   = "notify"
	mailLogTypeBatch    = "batch"
	mailLogTypeTest     = "test"
	mailLogTypeSchedule = "schedule"

	mailLogStatusSent   = "sent"
	mailLogStatusFailed = "failed"
)

// MailUser 是收件人解析需要的用户列（对齐 MailBatchService::recipientQuery 的 with('plan')）。
type MailUser struct {
	ID                  uint64
	Email               string
	Token               string
	PlanName            *string
	TrafficLimit        int64
	TrafficUsed         int64
	MonthlyTrafficUsed  int64
	MonthlyTrafficLimit int64
	ExpiredAt           *time.Time
}

// MailLogRow 是 mail_logs 一行。
//
// ToUserID 按模型 cast 输出 integer；列本身是 string，空串视作 null。
type MailLogRow struct {
	ID        uint64
	Type      string
	Status    string
	ToEmail   *string
	ToUserID  *int64
	Subject   *string
	Scene     *string
	BatchID   *int64
	Error     *string
	CreatedAt *time.Time
}

// MailLogFilter 是发信日志的筛选（GET /admin-api/email/logs）。
type MailLogFilter struct {
	Type   string // notify|batch|test|schedule，空 = 不筛
	Status string // sent|failed，空 = 不筛
}

// MailRecipientFilter 是批量发信的收件人筛选（mode=all|filter）。
type MailRecipientFilter struct {
	PlanID     *int64
	UserStatus string // normal|over|expired，空按 normal
}

// MailStore 是邮件域对数据库的全部依赖。
type MailStore interface {
	// SiteConfig 读写（键值表；缺失键读出空串，与 SiteConfig::getMany 一致）
	MailConfigMany(ctx context.Context, keys []string) (map[string]string, error)
	MailConfigSetMany(ctx context.Context, kv map[string]string) error

	// 收件人解析
	MailUserByLogin(ctx context.Context, query string) (*MailUser, error) // 邮箱精确或 ID 精确
	MailRecipients(ctx context.Context, f MailRecipientFilter, now time.Time) ([]MailUser, error)

	// 发信日志
	MailLogPage(ctx context.Context, f MailLogFilter, page, perPage int64) ([]MailLogRow, int64, error)
	CreateMailLog(ctx context.Context, row MailLogRow) error
}

// SMTPSettings 是一次发信要使用的 SMTP 配置（密码已解密）。
type SMTPSettings struct {
	Host       string
	Port       int
	Username   string
	Password   string
	Encryption string // tls|ssl|none
	FromName   string
}

// MailTransport 是发信接缝（对齐 SimpleMailerService::send / Mail::html）。
//
// Go 侧「forgetMailers」的等价物：**每次发送都由调用方把新解出来的 SMTPSettings 传进来**，
// 不在 Transport 内缓存连接配置 —— 改 mail 配置后下一封信立即生效，不会复用旧 mailer。
type MailTransport interface {
	Send(cfg SMTPSettings, to, subject, htmlBody string) error
}

// MailJob 是一封待发信（对齐 SendMailJob 的入参）。
type MailJob struct {
	ToEmail      string
	Subject      string
	HTMLBody     string
	Type         string // notify|batch|test|schedule
	UserID       *uint64
	BatchID      *int64
	Scene        *string
	DelaySeconds int
}

// MailQueue 是队列接缝（PHP 的 SendMailJob::dispatch）。
type MailQueue interface {
	Enqueue(job MailJob) error
}

// MailDeps 是邮件域的依赖清单。
type MailDeps struct {
	Store     MailStore
	Transport MailTransport
	Queue     MailQueue
	// AppKey 用于 Crypt::encryptString / decryptString（smtp_password）。
	AppKey []byte
	Now    func() time.Time
	Logger loggerLike
}

// MailDomain 是邮件域的 HTTP 入口。
type MailDomain struct {
	store     MailStore
	transport MailTransport
	queue     MailQueue
	appKey    []byte
	now       func() time.Time
	logger    loggerLike
}

// NewMailDomain 组装邮件域。Transport / Queue 为空时用真实实现（不是 noop 假成功）：
//   - Transport 空 → NewSMTPTransport（net/smtp 真实发信）；
//   - Queue 空 → newMailQueue（单 goroutine 串行 + DelaySeconds，执行体是 m.deliver）。
//
// 测试可注入 mock 覆盖两者；nil 不再退化为「记日志后假装成功」。
func NewMailDomain(d MailDeps) *MailDomain {
	if d.Store == nil {
		panic("admin: MailStore 不能为空")
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	logger := d.Logger
	if logger == nil {
		logger = log.Default()
	}
	transport := d.Transport
	if transport == nil {
		transport = NewSMTPTransport(logger)
	}
	m := &MailDomain{
		store:     d.Store,
		transport: transport,
		appKey:    d.AppKey,
		now:       now,
		logger:    logger,
	}
	queue := d.Queue
	if queue == nil {
		queue = newMailQueue(m.deliver, logger)
	}
	m.queue = queue
	return m
}

// MailQueue 暴露邮件队列（单 goroutine 串行），供 commandRunner 的定时通知邮件
// （mail:notify-scan / mail:schedule-scan）复用同一队列，避免各自起 worker 并发发信。
func (m *MailDomain) MailQueue() MailQueue { return m.queue }
