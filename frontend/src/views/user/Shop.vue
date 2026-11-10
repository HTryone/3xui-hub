<script setup>
/**
 * Shop — 套餐列表 + 订单管理
 */
import { ref, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import Icon from '@/components/ui/Icon.vue'
import Modal from '@/components/ui/Modal.vue'
import UserIcon from '@/components/user/UserIcon.vue'
import BalanceRechargeSheet from '@/components/user/BalanceRechargeSheet.vue'
import { useAuthStore } from '@/stores/auth'
import { userApi } from '@/api/user'
import { useToast } from '@/components/ui/Toast.vue'
import { formatYuan } from '@/utils/money'
import { beginPayWindow, gotoPay, cancelPayWindow } from '@/utils/paywindow'

const auth = useAuthStore()
const toast = useToast()
const router = useRouter()

const plans = ref([])
const paymentMethods = ref([])
const loading = ref(true)
const selectedPlan = ref(null)
const selectedPayment = ref(null)
const creatingOrder = ref(false)
const pollingOrder = ref(null)
const buyModalOpen = ref(false)
// 余额支付：余额（分）+ 充值弹层。余额是「能不能用余额付」的唯一判据，
// 每次打开购买弹窗都刷新一次（不能拿进页面时的旧余额做判断）。
const balanceCents = ref(0)
const rechargeOpen = ref(false)
// 移动端套餐布局：宫格（默认）/ 列表
const planView = ref('grid')
const VIEW_KEY = 'shop_plan_view'
try {
  const saved = localStorage.getItem(VIEW_KEY)
  if (saved === 'grid' || saved === 'list') planView.value = saved
} catch (e) { /* 隐私模式忽略 */ }
const setPlanView = (v) => {
  planView.value = v
  try { localStorage.setItem(VIEW_KEY, v) } catch (e) { /* 忽略 */ }
}

const user = computed(() => auth.user || {})

const fmtBytes = (b) => {
  if (!b || b < 0 || !Number.isFinite(b)) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB', 'EB']
  const raw = Math.floor(Math.log(b) / Math.log(1024))
  const i = Math.max(0, Math.min(raw, u.length - 1))
  return `${(b / Math.pow(1024, i)).toFixed(i ? 2 : 0)} ${u[i]}`
}
const fmtPrice = (p) => { if (!p && p !== 0) return '—'; return p > 0 ? `¥${p}` : '免费' }

const loadPlans = async () => { try { plans.value = await userApi.plans() } catch (e) { plans.value = [] } }
const loadPaymentMethods = async () => { try { paymentMethods.value = await userApi.paymentMethods() } catch (e) { paymentMethods.value = [] } }
// 余额（分）：查不到就按 0 —— 只影响「余额支付够不够」的提示，不阻断下单
const loadBalance = async () => {
  try { balanceCents.value = Number((await userApi.balance({ per_page: 1 }))?.balance || 0) } catch (e) { balanceCents.value = 0 }
}

const openBuy = async (p) => {
  selectedPlan.value = p
  selectedPayment.value = null
  resetDiscount()
  if (!paymentMethods.value.length) await loadPaymentMethods()
  await loadBalance()
  if (paymentMethods.value.length) selectedPayment.value = paymentMethods.value[0].id
  buyModalOpen.value = true
}

/* ===== 折扣码（邀请码 / 兑换码）：一个输入框，只能填一个码，不存在叠加 ===== */
const discountCode = ref('')
const discountChecking = ref(false)
const discountResult = ref(null)
let discountTimer = null

const resetDiscount = () => {
  discountCode.value = ''
  discountResult.value = null
  discountChecking.value = false
  if (discountTimer) { clearTimeout(discountTimer); discountTimer = null }
}

// 输入后防抖 400ms 调 /api/discount/check（仅校验，不占用次数）
const onDiscountInput = () => {
  if (discountTimer) clearTimeout(discountTimer)
  discountResult.value = null
  const code = discountCode.value.trim()
  if (!code || !selectedPlan.value || selectedPlan.value.price <= 0) {
    discountChecking.value = false
    return
  }
  discountChecking.value = true
  discountTimer = setTimeout(async () => {
    try {
      discountResult.value = await userApi.checkDiscount(code, selectedPlan.value.id)
    } catch (e) {
      discountResult.value = { usable: false, message: e.message || '优惠码校验失败' }
    } finally {
      discountChecking.value = false
    }
  }, 400)
}

const discountUsable = computed(() => !!discountResult.value?.usable)

// 价格主视觉 / 底部按钮金额都跟着折扣走
const priceInfo = computed(() => {
  const original = Number(selectedPlan.value?.price || 0)
  if (discountUsable.value && original > 0) {
    return { final: Number(discountResult.value.final_amount), original, discounted: true }
  }
  return { final: original, original, discounted: false }
})

const discountHint = computed(() => discountResult.value?.message || '')
const discountNote = computed(() => (discountUsable.value ? discountResult.value?.note : '') || '')

// 余额支付：能不能付由「余额（分）>= 折后金额（分）」决定，前端先拦一次，
// 真正的校验在后端（事务 + FOR UPDATE）。折后金额和 /api/discount/check 同源。
const isBalancePay = computed(() => selectedPayment.value === 'balance')
const balanceNeededCents = computed(() => Math.round(Number(priceInfo.value.final || 0) * 100))
const balanceEnough = computed(() => balanceCents.value >= balanceNeededCents.value)

const createOrder = async () => {
  if (!selectedPlan.value) { toast.error('请选择套餐'); return }
  if (isBalancePay.value && !balanceEnough.value) { toast.error('余额不足，请先充值'); return }
  // iOS Safari 会在「await 之后」拦截 window.open：先同步占住窗口再发请求（非 iOS 不占，行为不变）
  const payWin = beginPayWindow()
  creatingOrder.value = true
  let opened = false
  try {
    const code = discountCode.value.trim()
    const result = isBalancePay.value
      ? await userApi.createBalancePayOrder(selectedPlan.value.id, code)
      : await userApi.createOrder(selectedPlan.value.id, selectedPayment.value, code)
    if (result.status === 'paid') {
      toast.success(isBalancePay.value ? '支付成功，套餐已开通' : '购买成功（免费套餐）')
      buyModalOpen.value = false
      selectedPlan.value = null
      resetDiscount()
      await Promise.all([auth.fetchMe(), loadBalance()])
      return
    }
    if (result.pay_url) {
      buyModalOpen.value = false
      gotoPay(payWin, result.pay_url)
      opened = true
      toast.success('订单已创建，请完成支付')
      pollingOrder.value = result.order_no
      startPolling(result.order_no)
    }
  } catch (e) {
    toast.error(e.message || '创建订单失败')
  } finally {
    if (!opened) cancelPayWindow(payWin) // 没出支付链接/已支付直返：关掉预占的空白页
    creatingOrder.value = false
  }
}

const startPolling = (orderNo) => {
  let count = 0
  const timer = setInterval(async () => {
    count++
    try {
      const status = await userApi.orderStatus(orderNo)
      if (status.status === 'paid') {
        clearInterval(timer)
        pollingOrder.value = null
        toast.success('支付成功！')
        await auth.fetchMe()
        return
      }
    } catch (e) {}
    if (count >= 60) { clearInterval(timer); pollingOrder.value = null }
  }, 5000)
}

onMounted(async () => {
  await Promise.all([loadPlans(), loadPaymentMethods()])
  loading.value = false
})
</script>

<template>
  <div class="shop">
    <!-- 操作栏 -->
    <div class="shop-actions">
      <h2 class="shop-title">套餐</h2>
      <div class="shop-actions-right">
        <!-- 布局切换：仅移动端显示 -->
        <div class="shop-view-switch" role="group" aria-label="布局切换">
          <button
            type="button"
            class="shop-view-btn"
            :class="{ 'shop-view-btn--on': planView === 'grid' }"
            aria-label="宫格"
            @click="setPlanView('grid')"
          >
            <span class="shop-view-glyph shop-view-glyph--grid"></span>
          </button>
          <button
            type="button"
            class="shop-view-btn"
            :class="{ 'shop-view-btn--on': planView === 'list' }"
            aria-label="列表"
            @click="setPlanView('list')"
          >
            <span class="shop-view-glyph shop-view-glyph--list"></span>
          </button>
        </div>
        <button type="button" class="shop-orders-btn shop-recharge-btn uu-press" @click="rechargeOpen = true">
          <span>充值</span>
        </button>
        <button type="button" class="shop-orders-btn uu-press" @click="router.push({ name: 'user-orders' })">
          <UserIcon name="cart" :size="16" />
          <span>订单</span>
        </button>
      </div>
    </div>

    <!-- 套餐列表 -->
    <div v-if="loading" class="shop-plans">
      <div v-for="i in 3" :key="i" class="shop-skeleton">
        <div class="shop-skeleton-inner">
          <div class="shop-skeleton-bar shop-skeleton-bar--sm"></div>
          <div class="shop-skeleton-bar shop-skeleton-bar--xl"></div>
          <div class="shop-skeleton-bar shop-skeleton-bar--sm"></div>
          <div class="shop-skeleton-bar shop-skeleton-bar--line"></div>
          <div class="shop-skeleton-bar shop-skeleton-bar--line"></div>
          <div class="shop-skeleton-bar shop-skeleton-bar--line"></div>
          <div class="shop-skeleton-bar shop-skeleton-bar--btn"></div>
        </div>
      </div>
    </div>
    <div v-else class="shop-plans" :class="`shop-plans--${planView}`">
      <div v-for="(p, idx) in plans" :key="p.id" class="shop-plan-card" :class="{ 'shop-plan-card--highlight': idx === 1 }" @click="openBuy(p)">
        <div class="shop-plan-inner">
          <h3 class="shop-plan-name">{{ p.name }}</h3>
          <div class="shop-plan-price">
            <span class="shop-plan-currency">¥</span>
            <span class="shop-plan-amount">{{ p.price || 0 }}</span>
            <span v-if="p.type === 'period'" class="shop-plan-unit">{{ p.months === 1 ? '/月' : p.months === 3 ? '/季' : p.months === 12 ? '/年' : '/' + p.months + '个月' }}</span>
          </div>
          <p v-if="p.type === 'period' && p.months > 1" class="shop-plan-equiv">{{ (p.price / p.months).toFixed(1) }}元/月</p>
          <ul class="shop-plan-features">
            <template v-if="!p.include_local && p.third_party_sub_ids?.length">
              <li>{{ p.months }} 个月有效期</li>
              <li>高速线路可用</li>
              <li>不限速</li>
            </template>
            <template v-else-if="p.type === 'period'">
              <li>{{ p.months }} 个月有效期</li>
              <li>每月 {{ fmtBytes(p.monthly_traffic) }}</li>
              <li>流量按月重置</li>
              <li>全节点可用</li>
              <li>不限速</li>
            </template>
            <template v-else>
              <li>总量 {{ fmtBytes(p.total_traffic) }}</li>
              <li>永久有效</li>
              <li>用完为止</li>
              <li>全节点可用</li>
              <li>不限速</li>
            </template>
          </ul>
          <button class="shop-plan-btn" :class="idx === 1 ? 'shop-plan-btn--dark' : 'shop-plan-btn--light'" @click.stop="openBuy(p)">
            选择 {{ p.name }}
          </button>
        </div>
      </div>
      <div v-if="!plans.length && !loading" class="shop-empty">
        <div class="shop-empty-icon"><Icon name="package" :size="32" /></div>
        <p class="shop-empty-title">暂无套餐</p>
        <p class="shop-empty-desc">管理员尚未发布套餐，请稍后再来</p>
      </div>
    </div>

    <!-- 购买弹窗 -->
    <Modal v-model="buyModalOpen" :title="selectedPlan ? `购买 ${selectedPlan.name}` : '购买套餐'" width="460px">
      <!-- uu-root：Modal 被 Teleport 到 body，需要在此重新挂上用户端 token 作用域 -->
      <div v-if="selectedPlan" class="buy uu-root">
        <!-- 价格主视觉 -->
        <div class="buy-hero">
          <div class="buy-hero-main">
            <span class="buy-hero-price">{{ fmtPrice(priceInfo.final) }}</span>
            <span v-if="priceInfo.discounted" class="buy-hero-old">¥{{ priceInfo.original }}</span>
            <span class="buy-hero-unit">{{ selectedPlan.type === 'period' ? `/${selectedPlan.months}个月` : '一次性' }}</span>
          </div>
          <span class="buy-hero-tag" :class="{ 'buy-hero-tag--on': priceInfo.discounted }">
            {{ priceInfo.discounted ? `已优惠 ¥${(priceInfo.original - priceInfo.final).toFixed(2)}` : (selectedPlan.type === 'period' ? '周期套餐' : '总量套餐') }}
          </span>
        </div>

        <!-- 规格 -->
        <div class="buy-specs">
          <template v-if="!selectedPlan.include_local && selectedPlan.third_party_sub_ids?.length">
            <div class="buy-spec">
              <span class="buy-spec-k">线路</span>
              <span class="buy-spec-v">高速线路可用</span>
              <span class="buy-spec-hint">不限速</span>
            </div>
            <div class="buy-spec">
              <span class="buy-spec-k">有效期</span>
              <span class="buy-spec-v">{{ selectedPlan.months }} 个月</span>
              <span class="buy-spec-hint">到期后需续费</span>
            </div>
          </template>
          <template v-else>
            <div class="buy-spec">
              <span class="buy-spec-k">流量</span>
              <span class="buy-spec-v">{{ selectedPlan.type === 'period' ? fmtBytes(selectedPlan.monthly_traffic) : fmtBytes(selectedPlan.total_traffic) }}</span>
              <span class="buy-spec-hint">{{ selectedPlan.type === 'period' ? '每月重置' : '用完为止' }}</span>
            </div>
            <div class="buy-spec">
              <span class="buy-spec-k">有效期</span>
              <span class="buy-spec-v">{{ selectedPlan.type === 'period' ? `${selectedPlan.months} 个月` : '永久' }}</span>
              <span class="buy-spec-hint">{{ selectedPlan.type === 'period' ? '到期后需续费' : '不限时间' }}</span>
            </div>
          </template>
        </div>

        <p v-if="selectedPlan.type === 'period'" class="buy-note">
          重复购买同一套餐时，有效期将从本次购买重新计算，剩余天数不累计。
        </p>

        <!-- 邀请码 / 兑换码：一个输入框，只能填一个码 -->
        <div v-if="selectedPlan.price > 0" class="buy-disc">
          <p class="buy-pay-title">邀请码 / 兑换码</p>
          <div
            class="buy-disc-box"
            :class="{
              'buy-disc-box--ok': discountUsable,
              'buy-disc-box--err': discountResult && !discountUsable,
            }"
          >
            <input
              v-model="discountCode"
              class="buy-disc-input"
              type="text"
              autocomplete="off"
              spellcheck="false"
              placeholder="填写邀请码或兑换码"
              @input="onDiscountInput"
            />
            <span v-if="discountChecking" class="buy-disc-spin"><UserIcon name="refresh" :size="16" spin /></span>
            <span v-else-if="discountUsable" class="buy-disc-ok"><UserIcon name="checkCircle" :size="18" /></span>
          </div>
          <p v-if="discountHint" class="buy-disc-msg" :class="discountUsable ? 'buy-disc-msg--ok' : 'buy-disc-msg--err'">
            {{ discountHint }}
          </p>
          <p v-if="discountNote" class="buy-disc-note">{{ discountNote }}</p>
          <p v-if="discountUsable" class="buy-disc-price">
            折扣 {{ (discountResult.discount * 10).toFixed(1) }} 折 · 折后 <b>¥{{ priceInfo.final.toFixed(2) }}</b>
          </p>
        </div>

        <!-- 支付方式 -->
        <div v-if="selectedPlan.price > 0" class="buy-pay">
          <p class="buy-pay-title">支付方式</p>
          <div class="buy-pay-list">
            <!-- 余额支付排最前：不经过第三方网关，扣款即开通 -->
            <label
              class="buy-pay-item"
              :class="{ 'buy-pay-item--on': isBalancePay }"
            >
              <input type="radio" value="balance" v-model="selectedPayment" />
              <span class="buy-pay-name">余额支付</span>
              <span class="buy-pay-extra">余额 {{ formatYuan(balanceCents) }}</span>
              <span class="buy-pay-check">
                <Icon name="check" :size="13" />
              </span>
            </label>
            <label
              v-for="m in paymentMethods"
              :key="m.id"
              class="buy-pay-item"
              :class="{ 'buy-pay-item--on': selectedPayment === m.id }"
            >
              <input type="radio" :value="m.id" v-model="selectedPayment" />
              <span class="buy-pay-name">{{ m.name }}</span>
              <span class="buy-pay-check">
                <Icon name="check" :size="13" />
              </span>
            </label>
          </div>
          <p v-if="isBalancePay && !balanceEnough" class="buy-pay-warn">
            余额不足以支付 {{ formatYuan(balanceNeededCents) }}，请先充值
          </p>
        </div>
      </div>

      <template #footer>
        <div class="buy-foot uu-root">
          <button type="button" class="buy-btn" @click="buyModalOpen = false">取消</button>
          <button
            type="button"
            class="buy-btn buy-btn--primary"
            :disabled="creatingOrder || (isBalancePay && !balanceEnough)"
            @click="createOrder"
          >
            {{ creatingOrder ? '处理中...' : (selectedPlan && selectedPlan.price > 0 ? `支付 ${fmtPrice(priceInfo.final)}` : '免费领取') }}
          </button>
        </div>
      </template>
    </Modal>

    <!-- 充值弹层（余额不足时从支付方式区引导过来） -->
    <BalanceRechargeSheet v-model="rechargeOpen" @paid="loadBalance" />
  </div>
</template>

<style scoped src="@/assets/styles/pages/user/Shop.css"></style>
