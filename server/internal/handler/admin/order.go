package admin

// 管理端订单（/admin-api/orders）：列表分页 + 搜索。
//
// 逐条对齐 backend/app/Http/Controllers/Admin/OrderController.php。

import (
	"context"
	"time"

	"controlhub-server/internal/driver/payment"

	"github.com/gin-gonic/gin"
)

// OrderRow 是订单列表的一行（含预加载的关联展示字段）。
//
// 金额按 data-contract §3 以字符串持有，输出时按 PHP 的 (float) 走 phpFloat。
type OrderRow struct {
	ID           uint64
	OrderNo      string
	UserID       uint64
	UserEmail    *string
	PlanName     *string
	ProxySKUName *string
	OrderType    *string
	Amount       string // decimal(10,2)
	Status       string
	PaymentName  *string
	// DiscountCode 是这单用的优惠码文本（LEFT JOIN discount_codes）；NULL=没用码。
	// DiscountCodeDeleted 标记该码是否已被软删（status='deleted'）：展示「码已删除」而非悬空。
	DiscountCode        *string
	DiscountCodeDeleted bool
	// PaymentDriver 是 orders.payment_driver_type：余额支付单为 'balance'，
	// 它们没有 payment_config（PaymentName 为空），展示层靠它补「余额支付」。
	PaymentDriver *string
	TradeNo       *string
	PaidAt        *time.Time
	CreatedAt     *time.Time
	// PayExpiresAt 是支付链接失效时刻（下单时按全局支付有效期算好落库）；
	// 为空的老单按缺省有效期从 CreatedAt 兜底。列表据此**自己算**过期，
	// 不依赖兜底任务有没有跑过（任务只是低频扫地）。
	PayExpiresAt *time.Time
}

// OrderFilter 是列表筛选条件（order_type + search）。
type OrderFilter struct {
	OrderType string
	Search    string
}

// OrderStore 是订单域对数据库的依赖（SQL 实现见 order_sqlstore.go）。
type OrderStore interface {
	OrderPage(ctx context.Context, f OrderFilter, page, perPage int64) ([]OrderRow, int64, error)
}

// present 对齐 OrderController::index 的 map 闭包。
func presentOrder(o *OrderRow) *jsObj {
	planName := "-"
	if o.PlanName != nil {
		planName = *o.PlanName
	} else if o.ProxySKUName != nil {
		planName = "代理 · " + *o.ProxySKUName
	} else if o.OrderType != nil && *o.OrderType == "balance" {
		// 充值订单没有套餐/代理，之前落「-」看不出是什么单。
		planName = "余额充值"
	}
	paymentName := ""
	if o.PaymentName != nil {
		paymentName = *o.PaymentName
	}
	if paymentName == "" && o.PaymentDriver != nil && *o.PaymentDriver == "balance" {
		// 余额支付单没有 payment_config 关联，支付方式列按驱动标记补名。
		paymentName = "余额支付"
	}
	var payment any
	if paymentName != "" {
		payment = paymentName
	}
	orderType := any(nil)
	if o.OrderType != nil {
		orderType = *o.OrderType
	}
	// 支付窗口由我们自己的时钟判定：待支付但已到点 → 按「已过期」展示。
	// 与用户端、/api/payment/retry 同一套判据（payment.ExpiredAt + payment.DefaultTTL），
	// 因此不需要靠兜底任务勤跑来对齐显示。
	status := o.Status
	if status == "pending" && payWindowClosed(o.CreatedAt, o.PayExpiresAt) {
		status = "expired"
	}
	// 优惠码列：有码 → 码文本（已删则标「（码已删除）」）；没码 → null。
	var discountCode any
	if o.DiscountCode != nil {
		if o.DiscountCodeDeleted {
			discountCode = *o.DiscountCode + "（码已删除）"
		} else {
			discountCode = *o.DiscountCode
		}
	}
	return jo(
		"order_no", o.OrderNo,
		"user_id", o.UserID,
		"user_email", nullableStr(o.UserEmail),
		"plan_name", planName,
		"order_type", orderType,
		"amount", phpFloatOf(o.Amount),
		"status", status,
		"payment_name", payment,
		"discount_code", discountCode,
		"trade_no", nullableStr(o.TradeNo),
		"paid_at", nullableISO(o.PaidAt),
		"created_at", nullableISO(o.CreatedAt),
	)
}

// payWindowClosed 判定订单的支付窗口是否已过。
// 有 pay_expires_at 用它（下单时冻结的绝对时刻）；老单为空 → created_at + 缺省有效期。
func payWindowClosed(createdAt *time.Time, payExpiresAt *time.Time) bool {
	if payExpiresAt != nil {
		return !time.Now().Before(*payExpiresAt)
	}
	if createdAt == nil {
		return false
	}
	return !time.Now().Before(createdAt.Add(payment.DefaultTTL()))
}

// orderIndex 对齐 OrderController::index：50/页 + order_type 筛选 + 全表搜索。
func (d *UserDomain) orderIndex(c *gin.Context) {
	f := OrderFilter{}
	typ := queryString(c, "order_type")
	if typ != nil && (*typ == "plan" || *typ == "proxy") {
		f.OrderType = *typ
	}
	if s := queryString(c, "search"); s != nil {
		f.Search = phpTrimSpaceOnly(*s)
	}

	page := pageParam(c)
	rows, total, err := d.orders.OrderPage(ctxOf(c), f, page, userPagePerPage)
	if err != nil {
		d.failServer(c, "查询订单", err)
		return
	}
	lastPage := (total + userPagePerPage - 1) / userPagePerPage
	if lastPage < 1 {
		lastPage = 1
	}
	if page > lastPage {
		page = lastPage
		rows, total, err = d.orders.OrderPage(ctxOf(c), f, page, userPagePerPage)
		if err != nil {
			d.failServer(c, "查询订单", err)
			return
		}
	}
	items := make([]any, 0, len(rows))
	for i := range rows {
		items = append(items, presentOrder(&rows[i]))
	}
	d.successPage(c, items, page, userPagePerPage, total)
}
