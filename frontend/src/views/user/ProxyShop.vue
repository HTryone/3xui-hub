<script setup>
/**
 * 用户端 · 代理商城。
 *
 * 和「套餐商店」是两个独立产品线：
 *  - 套餐商店卖的是节点订阅（周期 + 流量）
 *  - 这里卖的是代理出口（住宅 IP / 共享 socks），不影响节点订阅
 *
 * 购买流程与套餐一致：选规格 → 选支付方式 → 跳收银台 → 轮询订单状态。
 */
import { ref, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Modal from '@/components/ui/Modal.vue'
import Icon from '@/components/ui/Icon.vue'
import { userApi } from '@/api/user'
import { useToast } from '@/components/ui/Toast.vue'
import { beginPayWindow, gotoPay, cancelPayWindow } from '@/utils/paywindow'

const router = useRouter()
const toast = useToast()

const products = ref([])
const paymentMethods = ref([])
const loading = ref(true)

const buyOpen = ref(false)
const selectedSku = ref(null)
const selectedPayment = ref(null)
const buying = ref(false)
const polling = ref(false)

const anySku = computed(() => products.value.some((p) => p.skus?.length))

const load = async () => {
  loading.value = true
  try {
    products.value = (await userApi.proxyProducts()) || []
  } catch (e) {
    toast.error(e.message || '加载失败')
  } finally {
    loading.value = false
  }
}

const openBuy = async (sku) => {
  selectedSku.value = sku
  if (!paymentMethods.value.length) {
    try {
      paymentMethods.value = (await userApi.paymentMethods()) || []
    } catch (e) {
      paymentMethods.value = []
    }
  }
  selectedPayment.value = paymentMethods.value[0]?.id ?? null
  buyOpen.value = true
}

const doBuy = async () => {
  if (!selectedSku.value) return
  if (paymentMethods.value.length && !selectedPayment.value) {
    toast.error('请选择支付方式')
    return
  }
  // iOS Safari 会在「await 之后」拦截 window.open：先同步占住窗口再发请求（非 iOS 不占）
  const payWin = beginPayWindow()
  buying.value = true
  let opened = false
  try {
    const result = await userApi.createProxyOrder(selectedSku.value.id, selectedPayment.value)
    if (result.status === 'paid') {
      toast.success('开通成功')
      buyOpen.value = false
      router.push('/dashboard/proxy/mine')
      return
    }
    if (result.pay_url) {
      buyOpen.value = false
      gotoPay(payWin, result.pay_url)
      opened = true
      toast.success('订单已创建，请在收银台完成支付')
      startPolling(result.order_no)
    } else {
      toast.error('获取支付链接失败，请稍后再试')
    }
  } catch (e) {
    toast.error(e.message || '下单失败')
  } finally {
    if (!opened) cancelPayWindow(payWin)
    buying.value = false
  }
}

const startPolling = (orderNo) => {
  polling.value = true
  let count = 0
  const timer = setInterval(async () => {
    count++
    if (count > 60) {
      clearInterval(timer)
      polling.value = false
      return
    }
    try {
      const status = await userApi.orderStatus(orderNo)
      if (status?.status === 'paid') {
        clearInterval(timer)
        polling.value = false
        toast.success('支付成功，代理已开通')
        router.push('/dashboard/proxy/mine')
      }
    } catch (e) {
      /* 轮询失败静默，等下一轮 */
    }
  }, 3000)
}

const skuMeta = (sku) =>
  [
    sku.traffic_gb > 0 ? `${sku.traffic_gb} GB` : '不限流量',
    `${sku.duration_days} 天`,
    sku.max_thread_count > 0 ? `${sku.max_thread_count} 并发` : null,
    sku.region || null,
  ]
    .filter(Boolean)
    .join(' · ')

const kindLabel = (kind) => (kind === 'socks' ? '共享 socks' : '独享账号')

onMounted(load)
</script>

<template>
  <div class="px">
    <div class="px-head">
      <div>
        <h1 class="px-title">代理商城</h1>
        <p class="px-sub">住宅 IP 与共享 socks，独立于节点套餐，不影响你的订阅。</p>
      </div>
      <Button variant="ghost" size="sm" @click="router.push('/dashboard/proxy/mine')">
        <Icon name="server" :size="15" /> 我的代理
      </Button>
    </div>

    <p v-if="polling" class="px-poll">支付完成后会自动跳转，请稍候…</p>

    <Card v-for="p in products" :key="p.id" class="px-card">
      <div class="px-phead">
        <h2 class="px-pname">{{ p.name }}</h2>
        <span v-if="p.subtitle" class="px-psub">{{ p.subtitle }}</span>
      </div>
      <p v-if="p.description" class="px-pdesc">{{ p.description }}</p>

      <div class="px-skus">
        <button
          v-for="sku in p.skus"
          :key="sku.id"
          class="px-sku"
          type="button"
          @click="openBuy(sku)"
        >
          <span class="px-sku-top">
            <span class="px-sku-name">{{ sku.name }}</span>
            <Badge variant="default">{{ kindLabel(sku.kind) }}</Badge>
          </span>
          <span class="px-sku-meta">{{ skuMeta(sku) }}</span>
          <span class="px-sku-foot">
            <span class="px-price">¥{{ sku.price }}</span>
            <span class="px-stock">剩 {{ sku.stock_available }} 份</span>
          </span>
        </button>
      </div>
    </Card>

    <Card v-if="!anySku && !loading" class="px-card">
      <p class="px-empty">暂时没有可购买的代理，看看别的吧。</p>
    </Card>

    <Modal v-model="buyOpen" title="确认购买" width="460px">
      <div v-if="selectedSku" class="px-buy">
        <div class="px-buy-row">
          <span class="px-buy-label">规格</span>
          <span>{{ selectedSku.name }}</span>
        </div>
        <div class="px-buy-row">
          <span class="px-buy-label">内容</span>
          <span>{{ skuMeta(selectedSku) }}</span>
        </div>
        <div class="px-buy-row">
          <span class="px-buy-label">金额</span>
          <span class="px-price">¥{{ selectedSku.price }}</span>
        </div>

        <div v-if="paymentMethods.length" class="px-pays">
          <span class="px-buy-label">支付方式</span>
          <div class="px-pay-list">
            <button
              v-for="m in paymentMethods"
              :key="m.id"
              class="px-pay"
              :class="{ active: selectedPayment === m.id }"
              type="button"
              @click="selectedPayment = m.id"
            >
              {{ m.name }}
            </button>
          </div>
        </div>

        <p class="px-tip">
          购买后可在「我的代理」查看账号密码与 IP。流量额度由上游计费，本站只做转发与展示。
        </p>
      </div>

      <template #footer>
        <div class="px-foot">
          <Button variant="ghost" size="sm" @click="buyOpen = false">取消</Button>
          <Button variant="primary" size="sm" :loading="buying" @click="doBuy">去支付</Button>
        </div>
      </template>
    </Modal>
  </div>
</template>

<style scoped src="@/assets/styles/pages/user/ProxyShop.css"></style>
