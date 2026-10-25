package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"math/big"
	"strconv"
	"strings"
)

// 本文件是 DiscountCodeService 里支付链路要用到的那部分：
// validateAndApply（只校验不占用）、consume（支付成功才占用）、rewardInviter（发兑换码）、
// 以及折扣码的限流判定（与 /api/discount/check 共用计数）。
//
// 三条不变量（改之前先读）：
//  1. **码本身无效**类错误（不存在/失效/过期/用满）共用同一句文案「优惠码无效或不可用」——
//     一旦区分开，攻击者就能从文案反推「这个码存在吗、走到哪一步了」，穷举空间大幅收敛。
//     「码有效但你不能用」类（自邀、已用过、非本人、限套餐、折后不足 1 元）保留明确文案。
//  2. 下单只把 discount_code_id 登记到订单上，**不占次数**；used_count+1 与邀请码锁人
//     只在支付成功（ConsumeDiscountCode → LockUserInviteCode）时发生。
//  3. 复用 pending 单前必须比对金额：管理员改过折扣后，旧单是旧价，直接复用等于按旧价收款。

const codeInvalidMsg = "优惠码无效或不可用"

// inviteMaxUses 对齐 DiscountCodeService::INVITE_MAX_USES（邀请码按「每人一次」限用，码本身不设总量上限）。
const inviteMaxUses = 999999

// discountCheckResult 对齐 validateAndApply 的返回数组。
type discountCheckResult struct {
	Discount   money
	Final      money
	Code       *DiscountCode
	ReuseOrder *Order
}

// validateAndApply 对齐 DiscountCodeService::validateAndApply：只校验，不占用。
func (s *PaymentService) validateAndApply(ctx context.Context, user *User, rawCode string, plan *PlanRow, originalAmount money) (*discountCheckResult, error) {
	// 1. 查码（统一转大写去空格）
	code, err := s.store.DiscountCodeByCode(ctx, phpStrToUpper(phpTrim(rawCode)))
	if err != nil || code == nil {
		return nil, NewPaymentError("%s", codeInvalidMsg)
	}

	cfg, cfgErr := s.discountSiteConfig(ctx)
	if cfgErr != nil {
		// 读不到配置时按「码无效」处理：宁可拒绝，也不在没有邀请开关判断的情况下放行。
		s.logf("读取折扣码配置失败: %v", cfgErr)
		return nil, NewPaymentError("%s", codeInvalidMsg)
	}

	// 2. 邀请码来源校验：总开关 → 不能自邀 → 一个用户一生只能用一次邀请码
	if code.Source == "invite" {
		if !cfg.inviteEnabled {
			return nil, NewPaymentError("邀请机制已关闭，邀请码暂不可用")
		}
		if code.UserID.Valid && code.UserID.Int64 == int64(user.ID) {
			return nil, NewPaymentError("不能使用自己的邀请码")
		}
		// users.used_invite_code_id 不在 P2 的 User 结构里（那是邀请模块的列），
		// 这里按「是否已锁邀请码」单独查一次，避免为了一个字段去改 P2 的类型。
		locked, lerr := s.store.UserInviteCodeLocked(ctx, user.ID)
		if lerr != nil {
			return nil, lerr
		}
		if locked {
			return nil, NewPaymentError("您已使用过邀请码，无法再次使用")
		}
	}

	// 3. 兑换码来源校验：仅限持有者本人
	if code.Source == "redeem" && (!code.UserID.Valid || code.UserID.Int64 != int64(user.ID)) {
		return nil, NewPaymentError("该兑换码仅限本人使用")
	}

	// 4. 状态校验（四种情况共用同一句文案）
	now := s.now()
	if code.Status != "active" {
		return nil, NewPaymentError("%s", codeInvalidMsg)
	}
	if code.ExpiresAt.Valid && code.ExpiresAt.Time.Before(now) {
		// 顺手把过期状态落库（幂等）
		if eerr := s.store.ExpireDiscountCode(ctx, code.ID); eerr != nil {
			s.logf("折扣码置过期失败 id=%d: %v", code.ID, eerr)
		}
		return nil, NewPaymentError("%s", codeInvalidMsg)
	}
	if code.UsedCount >= code.MaxUses {
		return nil, NewPaymentError("%s", codeInvalidMsg)
	}

	// 5. 套餐绑定校验
	if code.PlanID.Valid && code.PlanID.Int64 != int64(plan.ID) {
		return nil, NewPaymentError("该优惠码仅限指定套餐使用")
	}

	// 5.5 每用户次数校验：只数**已支付成功**的订单，pending 不占次数；去重维度不带 plan_id。
	//     邀请码天然每人一次（consume 时锁 used_invite_code_id），这里不重复拦。
	if code.Source != "invite" && code.MaxUsesPerUser > 0 {
		used, cerr := s.store.CountPaidOrdersWithCode(ctx, code.ID, int64(user.ID), 0)
		if cerr != nil {
			return nil, cerr
		}
		if used >= code.MaxUsesPerUser {
			return nil, NewPaymentError("该优惠码您已使用过，无法再次使用")
		}
	}

	// 6. 计算折后价：PHP 是 round($originalAmount * (float)$discount, 2)。
	//    这里的 roundPHP 与 PHP 8.4+ 的 round() 逐条一致（见 payment_money.go 的说明）。
	discountRate := parseFloatOr(code.Discount, 0)
	final := roundMoney(parseFloatOr(originalAmount.String(), 0) * discountRate)

	// 7. 折后价不足 1 元不允许使用
	if originalAmount.cents > 0 && final.cents < 100 {
		return nil, NewPaymentError("折后金额不足 1 元，无法使用优惠码")
	}

	// 8. 同一用户 + 同一张码 + 同一套餐，已有未支付的 pending 单 → 交给调用方复用。
	//
	//    与 PHP 一样**不带时间条件**：pending 单没有 TTL（占用后移到支付成功后，
	//    重复建单没有任何意义），旧 pending 单可以一直复用。
	//    但复用前必须比对金额：管理员改过折扣（或套餐改价）后，旧单仍是旧价，
	//    直接复用会让用户按改价前的金额支付，促销调价不即时生效。
	reuse, err := s.store.LatestPendingOrderWithCode(ctx, user.ID, plan.ID, code.ID)
	if err != nil && !IsNoRows(err) {
		return nil, err
	}
	if reuse != nil && reuse.Amount != final.String() {
		// 金额不一致 → 不复用，调用方会按当前折扣新建一笔。
		reuse = nil
	}

	return &discountCheckResult{
		Discount:   mustMoney(code.Discount),
		Final:      final,
		Code:       code,
		ReuseOrder: reuse,
	}, nil
}

// discountConfig 是折扣相关的 site_configs 取值。
type discountConfig struct {
	inviteEnabled  bool
	inviteDiscount float64
	redeemDiscount float64
}

// discountSiteConfig 对齐 DiscountCodeService 的三个静态取值：
// inviteEnabled = SiteConfig::getValue('invite_enabled','1') === '1'
// inviteDiscount = (float) getValue('invite_discount','0.90')
// redeemDiscount = (float) getValue('redeem_discount','0.90')
func (s *PaymentService) discountSiteConfig(ctx context.Context) (discountConfig, error) {
	out := discountConfig{inviteEnabled: false, inviteDiscount: 0.90, redeemDiscount: 0.90}
	if s.siteConfigs == nil {
		return out, nil
	}
	values, err := s.siteConfigs.SiteConfigMany(ctx,
		[]string{"invite_enabled", "invite_discount", "redeem_discount"})
	if err != nil {
		return out, err
	}
	// SiteConfig::getValue('invite_enabled','1') === '1'：
	// 行不存在或值为空串都取默认值 '1'（邀请机制默认开启）；只有显式 '0' 才是关闭
	// （后台保存时 boolean(false) 落成 '0'，见 DiscountCodeController）。
	if v := values["invite_enabled"]; v != "" {
		out.inviteEnabled = v == "1"
	} else {
		out.inviteEnabled = true
	}
	if v, ok := values["invite_discount"]; ok && v != "" {
		out.inviteDiscount = parseFloatOr(v, 0.90)
	}
	if v, ok := values["redeem_discount"]; ok && v != "" {
		out.redeemDiscount = parseFloatOr(v, 0.90)
	}
	return out, nil
}

// redeemDiscount 是「系统发放兑换码的折扣」（SiteConfig redeem_discount，缺省 0.90）。
func (s *PaymentService) redeemDiscount(ctx context.Context) (string, error) {
	cfg, err := s.discountSiteConfig(ctx)
	if err != nil {
		return "", err
	}
	return strconv.FormatFloat(cfg.redeemDiscount, 'f', 2, 64), nil
}

// ---- 折扣码限流（RateGuardService 的折扣段）----

// discountCheckBlocked 对齐 RateGuardService::discountCheckBlocked。
func (s *PaymentService) discountCheckBlocked(ctx context.Context, userID int64, ip string) (bool, error) {
	if s.rateGuard == nil {
		return false, nil
	}
	return s.rateGuard.CheckBlocked(ctx, userID, ip)
}

func (s *PaymentService) recordDiscountRequest(ctx context.Context, userID int64) error {
	if s.rateGuard == nil {
		return nil
	}
	return s.rateGuard.RecordRequest(ctx, userID)
}

func (s *PaymentService) recordDiscountFailure(ctx context.Context, userID int64, ip string) error {
	if s.rateGuard == nil {
		return nil
	}
	return s.rateGuard.RecordFailure(ctx, userID, ip)
}

func (s *PaymentService) clearDiscountFailures(ctx context.Context, userID int64) error {
	if s.rateGuard == nil {
		return nil
	}
	return s.rateGuard.ClearFailures(ctx, userID)
}

// ---- 兑换码生成（对齐 DiscountCode::generateCode）----

// codeAlphabet 对齐 DiscountCode::CODE_ALPHABET：Base32 去掉易混的 0 O 1 I L。
const codeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// codeLength 对齐 DiscountCode::CODE_LENGTH（8 位码体）。
const codeLength = 8

// generateDiscountCode 生成「3 位前缀 + 8 位 Base32」，并循环查重确保唯一。
//
// 对齐 PHP 的 randomBody：逐字节取随机数，>= limit 的重取，保证每个字符等概率。
func (s *PaymentService) generateDiscountCode(ctx context.Context, prefix string) (string, error) {
	size := int64(len(codeAlphabet))
	limit := int64(256/size) * size

	for attempt := 0; attempt < 20; attempt++ {
		var body strings.Builder
		for body.Len() < codeLength {
			var b [1]byte
			if _, err := rand.Read(b[:]); err != nil {
				return "", err
			}
			if int64(b[0]) >= limit {
				continue
			}
			body.WriteByte(codeAlphabet[int64(b[0])%size])
		}
		candidate := phpStrToUpper(prefix) + body.String()

		existing, err := s.store.DiscountCodeByCode(ctx, candidate)
		if err != nil {
			return "", err
		}
		if existing == nil {
			return candidate, nil
		}
	}
	return "", NewPaymentError("生成兑换码失败：连续冲突")
}

// ---- 未使用但保留：与 PHP 同口径的「每用户次数复核」 ----

// countPaidOrdersWithCode 是 CountPaidOrdersWithCode 的薄封装（consume 路径用）。
func (s *PaymentService) countPaidOrdersWithCode(ctx context.Context, codeID, userID int64, excludeOrderID uint64) (int64, error) {
	return s.store.CountPaidOrdersWithCode(ctx, codeID, userID, excludeOrderID)
}

// consumeDiscount 对齐 DiscountCodeService::consume（加锁 → 次数复核 → used_count+1）。
//
// 返回 (是否成功, 错误)。「码已用满」对调用方是**可吞掉**的业务失败：
// 用户钱已经付了，订单必须照常完成，只记日志（见 CompleteOrder）。
func (s *PaymentService) consumeDiscount(ctx context.Context, code *DiscountCode, user *User, excludeOrderID uint64) error {
	// 每用户次数复核与 used_count+1 在同一个事务/行锁里完成（见 PaymentStore.ConsumeDiscountCode）。
	_, err := s.store.ConsumeDiscountCode(ctx, code.ID, int64(user.ID), excludeOrderID)
	return err
}

// _ 保留 sql 包的引用（sql.NullString 在签名里出现）。
var _ = sql.NullString{}

// bigIntMod 是 crypto/rand 的辅助（保留给将来的批量码生成）。
var _ = big.NewInt
