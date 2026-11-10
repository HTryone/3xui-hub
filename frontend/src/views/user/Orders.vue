<script setup>
/**
 * Orders — 我的订单全屏页。
 * 数据来自 userApi.myOrders()（GET /api/payment/orders）。
 *
 * 支付窗口「自己计时」：后端在下单时按全局支付有效期（管理端「支付配置」页可改）
 * 算好每单的 `expires_at` 一并下发；前端只做**本地倒计时**（不轮询、不依赖定时任务），
 * 归零即把该行显示为「已过期」并隐藏「继续支付」——与后端判定同源，不会各算一套。
 *
 * 未过期（status=pending）的订单整行可点，点了重新拉取支付链接跳回支付平台
 * （POST /api/payment/retry；到点后端回 410，此时刷新列表该单变「已过期」）。
 * 打开新窗口用 utils/paywindow：iOS 同步预占窗口，其它平台行为不变。
 */
import { ref, onMounted, onUnmounted } from 'vue'
import { userApi } from '@/api/user'
import { useToast } from '@/components/ui/Toast.vue'
import { beginPayWindow, gotoPay, cancelPayWindow } from '@/utils/paywindow'

const toast = useToast()
const orders = ref([])
const loading = ref(true)
// 正在重新拉取支付链接的单号：防连点 + 显示「处理中…」
const paying = ref('')

const STATUS = {
  paid: { label: '已支付', color: 'var(--green)', bg: 'var(--green-t)' },
  pending: { label: '待支付', color: 'var(--orange)', bg: 'var(--orange-t)' },
  failed: { label: '失败', color: 'var(--red)', bg: 'var(--red-t)' },
  expired: { label: '已过期', color: 'var(--lbl3)', bg: 'var(--fill3)' },
}
const UNKNOWN = { label: '未知', color: 'var(--lbl3)', bg: 'var(--fill3)' }

/* ===== 倒计时 =====
 * 时间**只由服务端决定**，用户怎么折腾都不变：
 *   - 截止时刻是下单时算好、存在库里的绝对时间（expires_at / orders.pay_expires_at）；
 *   - 基线用服务端下发的 remaining_seconds（服务端时钟），
 *   - 递减用本地**单调时钟** performance.now()，不碰设备墙上时间。
 * 因此关浏览器 / 退出登录 / 重开 / 换设备 / 刷新，甚至设备时间被改坏，
 * 都只会得到同一个截止时刻减去真实流逝的剩余量 —— 既不会重置，也不会被延长。
 */
const nowTick = ref(0) // 每秒自增，驱动重算
let baseAt = 0 // 拿到数据那一刻的单调时刻
let baseTick = 0 // 与其配对的 nowTick
let ticker = null

// 剩余秒数：非待支付 → null；有服务端基线用基线，否则退回 expires_at 差值（老响应兼容）。
const remainingSec = (o) => {
  if (o.status !== 'pending') return null
  if (typeof o.remaining_seconds === 'number') {
    const elapsed = (nowTick.value - baseTick) / 1000
    return Math.max(0, Math.ceil(o.remaining_seconds - elapsed))
  }
  if (!o.expires_at) return null
  const end = new Date(o.expires_at).getTime()
  if (Number.isNaN(end)) return null
  return Math.max(0, Math.floor((end - Date.now()) / 1000))
}

// 未过期才可点：待支付 **且** 倒计时还有剩余、且后台开了「允许继续支付」。
// can_continue_pay 由后端随列表下发（管理端「支付配置」页可关，网关不支持复用链接时用）。
const canPay = (o) =>
  o.status === 'pending' && o.can_continue_pay !== false && (remainingSec(o) ?? 1) > 0

// 状态展示：待支付但本地已归零 → 就地显示「已过期」，不必等刷新。
const statusOf = (o) =>
  o.status === 'pending' && remainingSec(o) === 0 ? STATUS.expired : STATUS[o.status] || UNKNOWN

// mm:ss（倒计时用等宽数字，跳动时不抖动）
const fmtRemaining = (sec) =>
  `${String(Math.floor(sec / 60)).padStart(2, '0')}:${String(sec % 60).padStart(2, '0')}`

const fmtAmount = (a) => (a > 0 ? `¥${a}` : '免费')

const fmtTime = (o) => {
  const t = o.paid_at || o.created_at
  if (!t) return '—'
  const d = new Date(t)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleString('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  })
}

const load = async () => {
  try {
    orders.value = (await userApi.myOrders()) || []
  } catch (e) {
    orders.value = []
  } finally {
    // 记录基线：本次响应到达的本地单调时刻，与服务端给出的 remaining_seconds 配对
    baseAt = performance.now()
    baseTick = nowTick.value
    loading.value = false
  }
}

// 点击未过期订单 → 重新出支付链接 → 跳回支付平台
const payOrder = async (o) => {
  if (!canPay(o) || paying.value) return
  // iOS Safari 会在 await 之后拦截 window.open：先同步占窗口（非 iOS 不占，行为不变）
  const payWin = beginPayWindow()
  paying.value = o.order_no
  let opened = false
  try {
    const res = await userApi.retryPay(o.order_no)
    if (res?.pay_url) {
      gotoPay(payWin, res.pay_url)
      opened = true
    } else {
      toast.error('获取支付链接失败，请稍后再试')
    }
  } catch (e) {
    toast.error(e.message || '支付失败')
    // 410 = 支付窗口已过、后端已把该单作废 → 刷新列表让状态变成「已过期」
    if (e.code === 410) load()
  } finally {
    if (!opened) cancelPayWindow(payWin)
    paying.value = ''
  }
}

onMounted(() => {
  load()
  // 单调时钟推进（performance.now 不受系统时间调整影响），每秒一次
  ticker = setInterval(() => {
    nowTick.value = baseTick + (performance.now() - baseAt)
  }, 1000)
})
onUnmounted(() => {
  if (ticker) clearInterval(ticker)
})
</script>

<template>
  <div class="od uu-slide-up">
    <section class="od-card uu-mat-regular">
      <div v-if="orders.length" class="od-list">
        <div
          v-for="o in orders"
          :key="o.order_no"
          class="od-row"
          :class="{ 'od-row--payable': canPay(o) }"
          :role="canPay(o) ? 'button' : undefined"
          :tabindex="canPay(o) ? 0 : undefined"
          @click="payOrder(o)"
          @keyup.enter="payOrder(o)"
        >          <div class="od-main">
            <p class="od-plan">{{ o.plan_name || '套餐' }}</p>
            <p class="od-time">{{ fmtTime(o) }}</p>
          </div>
          <div class="od-side">
            <p class="od-amount">{{ fmtAmount(o.amount) }}</p>
            <div class="od-status">
              <span
                class="od-badge"
                :style="{ color: statusOf(o).color, background: statusOf(o).bg }"
              >
                {{ statusOf(o).label }}
              </span>
              <!-- 待支付倒计时：只在「允许继续支付」开着时才显示（关了整套逻辑一起收起） -->
              <span v-if="canPay(o) && remainingSec(o) !== null && remainingSec(o) > 0" class="od-left">
                剩 {{ fmtRemaining(remainingSec(o)) }}
              </span>
              <!-- 仅未过期订单可点续付：整行可点，这里给一个明确的动作按钮 -->
              <span
                v-if="canPay(o)"
                class="od-pay-btn"
                :class="{ 'is-busy': paying === o.order_no }"
              >
                {{ paying === o.order_no ? '打开中…' : '继续支付' }}<span class="od-pay-arrow">›</span>
              </span>
            </div>
          </div>
        </div>
      </div>
      <p v-else-if="!loading" class="od-empty">暂无订单</p>
    </section>
  </div>
</template>

<style scoped src="@/assets/styles/pages/user/Orders.css"></style>
