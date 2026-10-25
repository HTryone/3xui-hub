package admin

// 管理端支付配置域（P8c）的类型与依赖。
//
// 逐条对齐 backend/app/Http/Controllers/Admin/PaymentController.php：
// GET/POST/PUT/DELETE /admin-api/payments + GET /admin-api/payments/protocols。
// 金额口径见 contracts/data-contract.md §3（本域只存网关配置，无金额字段）。

import (
	"context"
	"log"
	"time"

	"controlhub-server/internal/driver/payment"
)

// PaymentRow 是 payment_configs 的一行（含 driver_config 解析结果）。
//
// driver_config 是 JSON 对象，值统一按字符串保存（PHP 侧 trim((string)$v) 后入库）；
// 老数据 driver_config 为空时展示层用旧列拼视图（presentDriverConfig）。
type PaymentRow struct {
	ID           uint64
	Name         string
	DriverType   string
	DriverConfig map[string]string

	Gateway      string
	QueryGateway *string
	MemberID     string
	APIKey       string
	NotifyURL    *string
	BankCode     string
	Enabled      bool

	CreatedAt *time.Time
}

// PaymentStore 是支付域对数据库的全部依赖（SQL 实现见 payment_sqlstore.go）。
//
// 刻意不与 P8d 的 Store 合并：各域在自己文件里声明窄接口，互不牵连。
type PaymentStore interface {
	PaymentsAll(ctx context.Context) ([]PaymentRow, error)
	PaymentByID(ctx context.Context, id uint64) (*PaymentRow, error)
	CreatePayment(ctx context.Context, in PaymentWrite) (*PaymentRow, error)
	UpdatePayment(ctx context.Context, id uint64, in PaymentWrite) (*PaymentRow, error)
	DeletePayment(ctx context.Context, id uint64) error
}

// PaymentWrite 是一次落库的完整列（对齐 PHP validateConfig 的 $out）。
//
// Legacy 只包含**本次要写**的旧列（键 = 列名）：更新时密文留空不修改 → 该键不出现，
// SQL 层自然不动列；新建时五列齐全（query_gateway 可为 nil）。
// NotifyURLSet / EnabledSet 区分「未传（更新时不动列）」与「显式写入」——
// PHP 用 array_key_exists 控制 forceFill 是否碰到这两列，这里用显式标志复刻。
type PaymentWrite struct {
	Name         string
	DriverType   string
	DriverConfig map[string]string

	Legacy map[string]*string

	NotifyURL    *string
	NotifyURLSet bool
	Enabled      bool
	EnabledSet   bool

	Now time.Time
}

// PaymentDeps 是支付域的依赖清单。
type PaymentDeps struct {
	Store PaymentStore
	// Registry 提供协议定义（protocols 清单 + driver_type 归一化）。
	Registry *payment.Registry
	Now      func() time.Time
	Logger   loggerLike
}

// PaymentDomain 是支付配置域的 HTTP 入口。
type PaymentDomain struct {
	store    PaymentStore
	registry *payment.Registry
	now      func() time.Time
	logger   loggerLike
}

// NewPaymentDomain 组装支付配置域。
func NewPaymentDomain(d PaymentDeps) *PaymentDomain {
	if d.Store == nil {
		panic("admin: PaymentStore 不能为空")
	}
	reg := d.Registry
	if reg == nil {
		reg = payment.NewRegistry()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	logger := d.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &PaymentDomain{store: d.Store, registry: reg, now: now, logger: logger}
}
