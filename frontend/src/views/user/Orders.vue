<script setup>
/**
 * Orders — 我的订单全屏页。
 * 数据来自 userApi.myOrders()（GET /api/payment/orders）。
 *
 * 未过期（status=pending）的订单可点击，点了重新拉取支付链接并跳回支付平台继续付款
 * （POST /api/payment/retry，失败若链接已失效会回 410，此时刷新列表——该单会变「已过期」）。
 * 打开新窗口用 utils/paywindow：iOS 同步预占窗口，其它平台行为不变。
 */
import { ref, onMounted } from 'vue'
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
const statusOf = (s) => STATUS[s] || { label: s || '未知', color: 'var(--lbl3)', bg: 'var(--fill3)' }

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

// 未过期（待支付）才可点：已支付/已过期/失败的订单点了没意义
const canPay = (o) => o.status === 'pending'

const load = async () => {
  try {
    orders.value = (await userApi.myOrders()) || []
  } catch (e) {
    orders.value = []
  } finally {
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
    // 410 = 链接已失效、后端已把该单作废 → 刷新列表让状态变成「已过期」
    if (e.code === 410) load()
  } finally {
    if (!opened) cancelPayWindow(payWin)
    paying.value = ''
  }
}

onMounted(load)
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
        >
          <div class="od-main">
            <p class="od-plan">{{ o.plan_name || '套餐' }}</p>
            <p class="od-time">{{ fmtTime(o) }}</p>
          </div>
          <div class="od-side">
            <p class="od-amount">{{ fmtAmount(o.amount) }}</p>
            <span
              class="od-badge"
              :style="{ color: statusOf(o.status).color, background: statusOf(o.status).bg }"
            >
              {{ statusOf(o.status).label }}
            </span>
            <span v-if="canPay(o)" class="od-pay-hint">
              {{ paying === o.order_no ? '正在打开支付…' : '点击继续支付 ›' }}
            </span>
          </div>
        </div>
      </div>
      <p v-else-if="!loading" class="od-empty">暂无订单</p>
    </section>
  </div>
</template>

<style scoped src="@/assets/styles/pages/user/Orders.css"></style>
