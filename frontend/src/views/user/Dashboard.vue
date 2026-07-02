<script setup>
/**
 * Dashboard — 统计卡片 + 在线节点
 */
import { ref, computed, onMounted, nextTick } from 'vue'
import Icon from '@/components/ui/Icon.vue'
import Modal from '@/components/ui/Modal.vue'
import { useAuthStore } from '@/stores/auth'
import { userApi } from '@/api/user'
import { useToast } from '@/components/ui/Toast.vue'
import QRCode from 'qrcode'
import { useSiteConfigStore } from '@/stores/siteConfig'

const auth = useAuthStore()
const toast = useToast()
const siteConfig = useSiteConfigStore()

// 公告弹窗
const announcementModalOpen = ref(false)
const dismissAnnouncement = () => {
  localStorage.setItem('announcement_dismissed', siteConfig.config.announcement)
  announcementModalOpen.value = false
}
const nodes = ref([])

const user = computed(() => auth.user || {})

const fmtBytes = (b) => {
  if (!b) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(b) / Math.log(1024))
  return `${(b / Math.pow(1024, i)).toFixed(i ? 2 : 0)} ${u[i]}`
}

const regionFlags = {
  '香港': '🇭🇰', 'HK': '🇭🇰', 'Hong Kong': '🇭🇰',
  '台湾': '🇹🇼', 'TW': '🇹🇼', 'Taiwan': '🇹🇼',
  '日本': '🇯🇵', 'JP': '🇯🇵', 'Japan': '🇯🇵',
  '韩国': '🇰🇷', 'KR': '🇰🇷', 'Korea': '🇰🇷',
  '新加坡': '🇸🇬', 'SG': '🇸🇬', 'Singapore': '🇸🇬',
  '美国': '🇺🇸', 'US': '🇺🇸', 'USA': '🇺🇸', 'America': '🇺🇸',
  '英国': '🇬🇧', 'UK': '🇬🇧', 'Britain': '🇬🇧',
  '德国': '🇩🇪', 'DE': '🇩🇪', 'Germany': '🇩🇪',
  '法国': '🇫🇷', 'FR': '🇫🇷', 'France': '🇫🇷',
  '加拿大': '🇨🇦', 'CA': '🇨🇦', 'Canada': '🇨🇦',
  '澳大利亚': '🇦🇺', 'AU': '🇦🇺', 'Australia': '🇦🇺',
  '印度': '🇮🇳', 'IN': '🇮🇳', 'India': '🇮🇳',
  '泰国': '🇹🇭', 'TH': '🇹🇭', 'Thailand': '🇹🇭',
  '马来西亚': '🇲🇾', 'MY': '🇲🇾', 'Malaysia': '🇲🇾',
  '越南': '🇻🇳', 'VN': '🇻🇳', 'Vietnam': '🇻🇳',
  '俄罗斯': '🇷🇺', 'RU': '🇷🇺', 'Russia': '🇷🇺',
  '巴西': '🇧🇷', 'BR': '🇧🇷', 'Brazil': '🇧🇷',
  '土耳其': '🇹🇷', 'TR': '🇹🇷', 'Turkey': '🇹🇷',
  '荷兰': '🇳🇱', 'NL': '🇳🇱', 'Netherlands': '🇳🇱',
  '中国': '🇨🇳', 'CN': '🇨🇳', 'China': '🇨🇳',
}
const getFlag = (name) => {
  if (!name) return '🌐'
  for (const [key, flag] of Object.entries(regionFlags)) {
    if (name.toUpperCase().includes(key.toUpperCase())) return flag
  }
  return '🌐'
}

const trafficPercent = computed(() => {
  const limit = user.value.traffic_limit || 0
  if (!limit) return 0
  return Math.min(100, Math.round((user.value.traffic_used / limit) * 100))
})
const monthlyPercent = computed(() => {
  const limit = user.value.monthly_traffic_limit || 0
  if (!limit) return 0
  return Math.min(100, Math.round((user.value.monthly_traffic_used / limit) * 100))
})
const expiredLabel = computed(() => {
  if (!user.value.expired_at) return '永不过期'
  return new Date(user.value.expired_at).toLocaleDateString()
})
const planLabel = computed(() => {
  if (!user.value.plan_name) return '无套餐'
  return user.value.plan_name
})

// 重置倒计时
const resetDaysLeft = computed(() => {
  if (!user.value.next_traffic_reset_at) return null
  const now = new Date()
  const resetAt = new Date(user.value.next_traffic_reset_at)
  const diff = Math.ceil((resetAt - now) / (1000 * 60 * 60 * 24))
  return diff > 0 ? diff : 0
})
const showResetCountdown = computed(() => {
  return user.value.plan_type === 'period' && user.value.plan_months > 1 && resetDaysLeft.value !== null
})

const syncing = ref(false)
const syncTraffic = async () => {
  if (syncing.value) return
  syncing.value = true
  try {
    await userApi.syncTraffic()
    await auth.fetchMe()
    toast.success('流量已同步')
  } catch (e) {
    toast.error(e.message || '同步失败')
  } finally {
    syncing.value = false
  }
}

const loadNodes = async () => {
  try { nodes.value = await userApi.nodes() } catch (e) { nodes.value = [] }
}

// 订阅地址
const subUrl = computed(() => {
  if (!user.value.token) return ''
  return `${location.origin}/api/sub/${user.value.token}`
})

// 不同格式的订阅链接
const subUrlClash = computed(() => {
  if (!user.value.token) return ''
  return `${location.origin}/api/sub/${user.value.token}?clash=1`
})

const subUrlSingbox = computed(() => {
  if (!user.value.token) return ''
  return `${location.origin}/api/sub/${user.value.token}?singbox=1`
})

// 订阅格式配置
const subscriptionConfig = ref({
  sub_clash_enabled: '0',
  sub_singbox_enabled: '0',
})

const loadSubscriptionConfig = async () => {
  try {
    const data = await userApi.siteConfig()
    if (data) {
      subscriptionConfig.value = {
        sub_clash_enabled: data.sub_clash_enabled || '0',
        sub_singbox_enabled: data.sub_singbox_enabled || '0',
      }
    }
  } catch (e) {
    // 静默失败
  }
}

const copied = ref(false)
const copiedClash = ref(false)
const copiedSingbox = ref(false)

const copySub = async () => {
  if (!subUrl.value) return
  try {
    await navigator.clipboard.writeText(subUrl.value)
    copied.value = true
    toast.success('订阅地址已复制')
    setTimeout(() => { copied.value = false }, 1500)
  } catch (e) { toast.error('复制失败') }
}

const copySubClash = async () => {
  if (!subUrlClash.value) return
  try {
    await navigator.clipboard.writeText(subUrlClash.value)
    copiedClash.value = true
    toast.success('Clash 订阅地址已复制')
    setTimeout(() => { copiedClash.value = false }, 1500)
  } catch (e) { toast.error('复制失败') }
}

const copySubSingbox = async () => {
  if (!subUrlSingbox.value) return
  try {
    await navigator.clipboard.writeText(subUrlSingbox.value)
    copiedSingbox.value = true
    toast.success('Sing-box 订阅地址已复制')
    setTimeout(() => { copiedSingbox.value = false }, 1500)
  } catch (e) { toast.error('复制失败') }
}

// 二维码弹窗
const qrModalOpen = ref(false)
const qrCanvas = ref(null)
const openQR = async () => {
  if (!subUrl.value) return
  qrModalOpen.value = true
  await nextTick()
  if (qrCanvas.value) {
    await QRCode.toCanvas(qrCanvas.value, subUrl.value, { width: 220, margin: 2, color: { dark: '#1d1d1f', light: '#ffffff' } })
  }
}

// 重置流量
const paymentMethods = ref([])
const resetModalOpen = ref(false)
const resetPayment = ref(null)
const creatingReset = ref(false)
const resetPrice = computed(() => user.value.plan_reset_price || 0)

const loadPaymentMethods = async () => { try { paymentMethods.value = await userApi.paymentMethods() } catch (e) { paymentMethods.value = [] } }

const openReset = async () => {
  resetModalOpen.value = true
  resetPayment.value = null
  if (!paymentMethods.value.length) await loadPaymentMethods()
  if (paymentMethods.value.length) resetPayment.value = paymentMethods.value[0].id
}

const createResetOrder = async () => {
  creatingReset.value = true
  try {
    const result = await userApi.createResetOrder(resetPayment.value)
    if (result.status === 'paid') {
      toast.success('重置成功')
      resetModalOpen.value = false
      await auth.fetchMe()
      return
    }
    if (result.pay_url) {
      window.open(result.pay_url, '_blank')
      toast.success('订单已创建，请完成支付')
    }
  } catch (e) {
    toast.error(e.message || '创建订单失败')
  } finally {
    creatingReset.value = false
  }
}

onMounted(async () => {
  loadNodes()
  loadSubscriptionConfig()
  await siteConfig.fetch()
  const ann = siteConfig.config.announcement
  const dismissed = localStorage.getItem('announcement_dismissed')
  if (ann && dismissed !== ann) {
    announcementModalOpen.value = true
  }
})
</script>

<template>
  <div class="db">
    <!-- 欢迎区 -->
    <div class="db-welcome">
      <div class="db-welcome-left">
        <div class="db-avatar">{{ (user.email?.[0] || 'U').toUpperCase() }}</div>
        <div class="db-welcome-text">
          <h1 class="db-welcome-title">你好，{{ user.email?.split('@')[0] || '用户' }}</h1>
        <p class="db-welcome-sub">欢迎回来，这是你的控制面板</p>
        </div>
      </div>
      <div class="db-welcome-btns">
        <button class="db-sync-btn" :disabled="syncing" @click="syncTraffic">
          <Icon name="refresh" :size="16" />
          {{ syncing ? '同步中...' : '同步流量' }}
        </button>
        <button v-if="user.plan_type === 'period' && user.monthly_traffic_limit" class="db-reset-btn" @click="openReset">
          <Icon name="refresh" :size="16" /> 重置流量
        </button>
      </div>
    </div>

    <!-- 统计卡片 -->
    <div class="db-stats">
      <div class="db-stat-card">
        <div class="db-stat-icon db-stat-icon--blue"><Icon name="traffic" :size="20" /></div>
        <div class="db-stat-info">
          <span class="db-stat-label">{{ user.plan_type === 'period' ? '当月已用' : '已用流量' }}</span>
          <span class="db-stat-value">
            {{ fmtBytes(user.plan_type === 'period' ? user.monthly_traffic_used : user.traffic_used) }}
            <template v-if="user.plan_type === 'period' ? user.monthly_traffic_limit : user.traffic_limit">
              <span class="db-stat-limit">/ {{ fmtBytes(user.plan_type === 'period' ? user.monthly_traffic_limit : user.traffic_limit) }}</span>
            </template>
          </span>
        </div>
        <div class="db-stat-bar">
          <div class="db-stat-bar-fill" :style="{ width: (user.plan_type === 'period' ? monthlyPercent : trafficPercent) + '%' }"></div>
        </div>
      </div>

      <div class="db-stat-card">
        <div class="db-stat-icon db-stat-icon--purple"><Icon name="shield" :size="20" /></div>
        <div class="db-stat-info">
          <span class="db-stat-label">套餐</span>
          <span class="db-stat-value">{{ planLabel }}</span>
        </div>
      </div>

      <div class="db-stat-card">
        <div class="db-stat-icon db-stat-icon--orange"><Icon name="order" :size="20" /></div>
        <div class="db-stat-info">
          <span class="db-stat-label">到期时间</span>
          <span class="db-stat-value">{{ expiredLabel }}</span>
        </div>
      </div>

      <div v-if="showResetCountdown" class="db-stat-card">
        <div class="db-stat-icon db-stat-icon--cyan"><Icon name="refresh" :size="20" /></div>
        <div class="db-stat-info">
          <span class="db-stat-label">流量重置</span>
          <span class="db-stat-value">{{ resetDaysLeft }} 天后</span>
        </div>
      </div>

    </div>

    <!-- 订阅地址 -->
    <div class="db-card" v-if="subUrl">
      <div class="db-card-head">
        <h3 class="db-card-title">订阅地址</h3>
      </div>
      <div class="sub-url-box">
        <code class="sub-url">{{ subUrl }}</code>
      </div>
      <div class="sub-actions">
        <button class="sub-action-btn" :class="{ 'sub-action-btn--ok': copied }" @click="copySub">
          <Icon :name="copied ? 'check' : 'copy'" :size="14" /> {{ copied ? '已复制' : '复制' }}
        </button>
        <button class="sub-action-btn" @click="openQR">
          <Icon name="mail" :size="14" /> 二维码
        </button>
      </div>

      <!-- 客户端订阅链接 -->
      <div v-if="subscriptionConfig.sub_clash_enabled === '1' || subscriptionConfig.sub_singbox_enabled === '1'" class="sub-formats">
        <h4 class="sub-formats-title">客户端订阅链接</h4>

        <div class="sub-formats-grid">
          <div v-if="subscriptionConfig.sub_clash_enabled === '1'" class="sub-format-item" @click="copySubClash">
            <img src="/images/clash.png" alt="Clash" class="sub-format-icon" />
            <span class="sub-format-name">Clash / FlClash</span>
            <span class="sub-format-status">{{ copiedClash ? '已复制' : '点击复制' }}</span>
          </div>

          <div v-if="subscriptionConfig.sub_singbox_enabled === '1'" class="sub-format-item" @click="copySubSingbox">
            <img src="/images/singbox.svg" alt="Sing-box" class="sub-format-icon" />
            <span class="sub-format-name">Sing-box / NekoBox</span>
            <span class="sub-format-status">{{ copiedSingbox ? '已复制' : '点击复制' }}</span>
          </div>
        </div>
      </div>
    </div>

    <!-- 在线节点 -->
    <div class="db-card">
      <div class="db-card-head">
        <h3 class="db-card-title">在线节点</h3>
        <span class="db-card-count">{{ nodes.length }}</span>
      </div>
      <div v-if="nodes.length" class="db-nodes">
        <div v-for="n in nodes" :key="n.id" class="db-node">
          <span class="db-node-dot"></span>
          <span class="db-node-flag">{{ getFlag(n.name) }}</span>
          <span class="db-node-name">{{ n.name }}</span>
          <span class="db-node-latency">{{ n.latency }}ms</span>
        </div>
      </div>
      <p v-else class="db-empty">暂无可用节点</p>
    </div>

    <!-- 重置弹窗 -->
    <Modal v-model="resetModalOpen" title="重置流量" width="460px">
      <div class="db-reset-info">
        <p>此操作将重置已使用的流量但不会增加套餐时长，是否继续？</p>
      </div>
      <div v-if="resetPrice > 0 && paymentMethods.length" class="db-modal-pay">
        <p class="db-pay-title">选择支付方式</p>
        <div class="db-pay-list">
          <label v-for="m in paymentMethods" :key="m.id" class="db-pay-item" :class="{ 'db-pay-item--selected': resetPayment === m.id }">
            <input type="radio" :value="m.id" v-model="resetPayment" />
            <span>{{ m.name }}</span>
          </label>
        </div>
      </div>
      <template #footer>
        <div class="db-modal-footer">
          <button class="db-modal-btn" @click="resetModalOpen = false">取消</button>
          <button class="db-modal-btn db-modal-btn--primary" :disabled="creatingReset" @click="createResetOrder">
            {{ creatingReset ? '处理中...' : (resetPrice > 0 ? `支付 ¥${resetPrice}` : '免费重置') }}
          </button>
        </div>
      </template>
    </Modal>

    <!-- 二维码弹窗 -->
    <Modal v-model="qrModalOpen" title="扫码订阅" width="360px">
      <div class="sub-qr-wrap">
        <canvas ref="qrCanvas" class="sub-qr-canvas"></canvas>
      </div>
      <p class="sub-qr-hint">使用手机客户端扫描二维码即可自动导入订阅。</p>
    </Modal>

    <!-- 公告弹窗 -->
    <Modal v-model="announcementModalOpen" title="系统公告" width="480px" persistent>
      <div class="db-announcement">{{ siteConfig.config.announcement }}</div>
      <template #footer>
        <button class="db-modal-btn db-announce-dismiss" @click="dismissAnnouncement">不再提示</button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.db { padding-top: 8px; display: flex; flex-direction: column; gap: 20px; }

/* 欢迎区 */
.db-welcome {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 28px 32px;
  background: linear-gradient(135deg, #1d1d1f 0%, #2d2d30 50%, #1d1d1f 100%);
  background-size: 200% 200%;
  animation: welcome-gradient 8s ease infinite;
  border-radius: 20px;
  color: #fff;
  position: relative;
  overflow: hidden;
}
.db-welcome::before {
  content: '';
  position: absolute;
  top: -50%;
  right: -30%;
  width: 300px;
  height: 300px;
  background: radial-gradient(circle, rgba(255,255,255,0.06) 0%, transparent 70%);
  border-radius: 50%;
}
@keyframes welcome-gradient {
  0%, 100% { background-position: 0% 50%; }
  50% { background-position: 100% 50%; }
}
.db-welcome-left { display: flex; align-items: center; gap: 16px; }
.db-avatar {
  width: 48px; height: 48px; border-radius: 14px;
  background: rgba(255,255,255,0.15);
  display: flex; align-items: center; justify-content: center;
  font-size: 20px; font-weight: 800; color: #fff;
  flex-shrink: 0; text-transform: uppercase;
  backdrop-filter: blur(10px);
}
.db-welcome-title { margin: 0 0 4px; font-size: 22px; font-weight: 800; letter-spacing: -0.02em; }
.db-welcome-sub { margin: 0; font-size: 14px; color: rgba(255,255,255,0.6); }
.db-sync-btn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 10px 20px;
  border-radius: 999px;
  font-size: 13px;
  font-weight: 600;
  border: 1px solid rgba(255,255,255,0.2);
  background: rgba(255,255,255,0.1);
  color: #fff;
  cursor: pointer;
  transition: all 0.2s;
  font-family: inherit;
  backdrop-filter: blur(10px);
}
.db-sync-btn:hover { background: rgba(255,255,255,0.2); }
.db-sync-btn:disabled { opacity: 0.5; cursor: not-allowed; }

/* 统计卡片 */
.db-stats { display: grid; grid-template-columns: repeat(4, 1fr); gap: 16px; }
.db-stat-card {
  background: #fff;
  border: 1px solid rgba(0,0,0,0.04);
  border-radius: 16px;
  padding: 20px;
  box-shadow: 0 2px 12px -2px rgba(0,0,0,0.06);
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.db-stat-icon {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 40px;
  height: 40px;
  border-radius: 12px;
}
.db-stat-icon--blue { background: linear-gradient(135deg, #dbeafe, #eff6ff); color: #2563eb; }
.db-stat-icon--purple { background: linear-gradient(135deg, #ede9fe, #f3e8ff); color: #9333ea; }
.db-stat-icon--orange { background: linear-gradient(135deg, #ffedd5, #fff7ed); color: #ea580c; }
.db-stat-icon--green { background: linear-gradient(135deg, #dcfce7, #f0fdf4); color: #16a34a; }
.db-stat-icon--cyan { background: linear-gradient(135deg, #cffafe, #ecfeff); color: #0891b2; }
.db-stat-info { display: flex; flex-direction: column; gap: 2px; }
.db-stat-label { font-size: 12px; color: #999; font-weight: 500; }
.db-stat-value { font-size: 18px; font-weight: 800; color: #1d1d1f; letter-spacing: -0.02em; }
.db-stat-limit { font-size: 13px; font-weight: 500; color: #999; }
.db-stat-bar { height: 4px; background: rgba(0,0,0,0.04); border-radius: 2px; overflow: hidden; }
.db-stat-bar-fill { height: 100%; background: #2563eb; border-radius: 2px; transition: width 0.6s cubic-bezier(0.16, 1, 0.3, 1); }
.db-status-ok { color: #16a34a; }
.db-status-err { color: #dc2626; }

/* 卡片 */
.db-card {
  background: #fff;
  border: 1px solid rgba(0,0,0,0.04);
  border-radius: 16px;
  padding: 24px;
  box-shadow: 0 2px 12px -2px rgba(0,0,0,0.06);
}
.db-card-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 16px; }
.db-card-title { margin: 0; font-size: 15px; font-weight: 700; color: #1d1d1f; }
.db-card-count {
  font-size: 12px;
  font-weight: 700;
  color: #2563eb;
  background: #eff6ff;
  padding: 2px 10px;
  border-radius: 999px;
}

/* 节点 */
.db-nodes { display: flex; flex-direction: column; gap: 8px; }
.db-node {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 16px;
  background: #f8f9fa;
  border-radius: 12px;
  transition: background 0.2s;
}
.db-node:hover { background: #f0f0f0; }
.db-node-dot { width: 8px; height: 8px; border-radius: 50%; background: #16a34a; flex-shrink: 0; }
.db-node-flag { font-size: 16px; flex-shrink: 0; }
.db-node-name { flex: 1; font-size: 14px; font-weight: 600; color: #1d1d1f; }
.db-node-latency { font-size: 12px; font-weight: 600; color: #999; font-family: -apple-system, monospace; }
.db-empty { text-align: center; color: #999; font-size: 13px; padding: 24px; }

/* 响应式 */
@media (max-width: 768px) {
  .db-stats { grid-template-columns: repeat(2, 1fr); }
  .db-welcome { flex-direction: column; gap: 16px; text-align: center; padding: 24px; }
}
@media (max-width: 480px) {
  .db-stats { grid-template-columns: repeat(2, 1fr); gap: 10px; }
  .db-stat-card { padding: 14px; }
  .db-stat-value { font-size: 16px; }
}

/* 重置弹窗 */
.db-welcome-btns { display: flex; gap: 8px; }
.db-reset-btn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 10px 20px;
  border-radius: 999px;
  font-size: 13px;
  font-weight: 600;
  border: 1px solid rgba(255,255,255,0.2);
  background: rgba(255,120,0,0.2);
  color: #ffb366;
  cursor: pointer;
  transition: all 0.2s;
  font-family: inherit;
  backdrop-filter: blur(10px);
}
.db-reset-btn:hover { background: rgba(255,120,0,0.35); }
.db-reset-info { font-size: 14px; color: #1d1d1f; line-height: 1.6; margin-bottom: 16px; }
.db-pay-title { font-size: 13px; font-weight: 600; color: #666; margin-bottom: 10px; }
.db-modal-pay { margin-bottom: 16px; }
.db-pay-list { display: flex; gap: 8px; flex-wrap: wrap; margin-bottom: 12px; }
.db-pay-item {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 14px;
  background: #f5f5f5;
  border: 2px solid transparent;
  border-radius: 10px;
  cursor: pointer;
  font-size: 13px;
  transition: all 0.2s;
}
.db-pay-item--selected { border-color: #2563eb; }
.db-modal-footer { display: flex; justify-content: space-between; width: 100%; gap: 12px; }
.db-modal-btn {
  padding: 10px 24px;
  border-radius: 999px;
  font-size: 14px;
  font-weight: 600;
  border: 1px solid rgba(0,0,0,0.08);
  background: #fff;
  color: #1d1d1f;
  cursor: pointer;
  transition: all 0.2s;
  font-family: inherit;
}
.db-modal-btn:hover { background: #f5f5f5; }
.db-modal-btn--primary { background: #2563eb; color: #fff; border-color: #2563eb; }
.db-modal-btn--primary:hover { background: #1d4ed8; }
.db-modal-btn:disabled { opacity: 0.5; cursor: not-allowed; }

/* 订阅地址 */
.sub-url-box {
  display: flex; align-items: center; gap: 8px;
  background: #f8f9fa; border: 1px solid rgba(0,0,0,0.04);
  border-radius: 12px; padding: 14px 16px; margin-bottom: 14px;
}
.sub-url { flex: 1; font-size: 13px; color: #1d1d1f; word-break: break-all; font-family: -apple-system, SF Mono, monospace; line-height: 1.6; }
.sub-actions { display: flex; gap: 10px; }
.sub-action-btn {
  display: inline-flex; align-items: center; gap: 6px;
  padding: 8px 16px; border-radius: 999px; font-size: 13px; font-weight: 600;
  border: 1px solid rgba(0,0,0,0.08); background: #fff; color: #1d1d1f;
  cursor: pointer; transition: all 0.2s; font-family: inherit;
}
.sub-action-btn:hover { background: #f5f5f5; }
.sub-action-btn--ok { color: #16a34a; border-color: #bbf7d0; background: #f0fdf4; }
.sub-action-btn--ok { color: #16a34a; border-color: #16a34a; background: #f0fdf4; }
.sub-qr-wrap { display: flex; justify-content: center; align-items: center; padding: 24px; background: #fff; border-radius: 12px; }
.sub-qr-canvas { border-radius: 8px; }
.sub-qr-hint { text-align: center; font-size: 13px; color: #999; margin-top: 12px; }

/* 客户端订阅格式 */
.sub-formats {
  margin-top: 16px;
  padding-top: 16px;
  border-top: 1px solid rgba(0,0,0,0.06);
}
.sub-formats-title {
  font-size: 13px;
  font-weight: 600;
  color: #666;
  margin: 0 0 12px 0;
}
.sub-formats-grid {
  display: grid;
  grid-template-columns: repeat(2, 1fr);
  gap: 10px;
}
.sub-format-item {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  padding: 16px 12px;
  background: #f8f9fa;
  border: 1px solid rgba(0,0,0,0.04);
  border-radius: 12px;
  cursor: pointer;
  transition: all 0.2s;
}
.sub-format-item:hover {
  background: #f0f0f0;
  border-color: rgba(0,0,0,0.08);
}
.sub-format-icon {
  width: 32px;
  height: 32px;
  object-fit: contain;
}
.sub-format-name {
  font-size: 13px;
  font-weight: 600;
  color: #1d1d1f;
  text-align: center;
}
.sub-format-status {
  font-size: 11px;
  color: #999;
}

/* 反馈按钮 */
.db-actions { display: flex; gap: 10px; }
.db-action-btn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 8px 16px;
  border-radius: 999px;
  font-size: 13px;
  font-weight: 600;
  border: 1px solid rgba(0,0,0,0.08);
  background: #fff;
  color: #1d1d1f;
  cursor: pointer;
  transition: all 0.2s;
  text-decoration: none;
}
.db-action-btn:hover { background: #f5f5f5; }

/* 公告 */
.db-announcement { white-space: pre-wrap; line-height: 1.8; font-size: var(--text-sm, 14px); color: #1d1d1f; }
.db-announce-dismiss { background: #fff; color: #1d1d1f; border: 1px solid rgba(0,0,0,0.08); border-radius: 9999px; padding: 12px 24px; font-family: inherit; font-size: 14px; font-weight: 600; transition: all 0.3s; }
.db-announce-dismiss:hover { background: #f5f5f5; }

/* 暗色模式 */
html.dark .db-stat-card { background: rgba(20,20,20,0.8); border-color: rgba(255,255,255,0.06); }
html.dark .db-stat-icon--blue { background: rgba(37,99,235,0.15); color: #60a5fa; }
html.dark .db-stat-icon--purple { background: rgba(147,51,234,0.15); color: #c084fc; }
html.dark .db-stat-icon--orange { background: rgba(234,88,12,0.15); color: #fb923c; }
html.dark .db-stat-icon--green { background: rgba(22,163,74,0.15); color: #4ade80; }
html.dark .db-stat-label { color: #888; }
html.dark .db-stat-value { color: #fff; }
html.dark .db-stat-limit { color: #888; }
html.dark .db-stat-bar { background: rgba(255,255,255,0.06); }
html.dark .db-stat-bar-fill { background: #60a5fa; }
html.dark .db-status-ok { color: #4ade80; }
html.dark .db-status-err { color: #f87171; }
html.dark .db-card { background: rgba(20,20,20,0.8); border-color: rgba(255,255,255,0.06); }
html.dark .db-card-title { color: #fff; }
html.dark .db-card-count { background: rgba(37,99,235,0.15); color: #60a5fa; }
html.dark .db-node { background: rgba(255,255,255,0.04); }
html.dark .db-node:hover { background: rgba(255,255,255,0.08); }
html.dark .db-node-dot { background: #4ade80; }
html.dark .db-node-name { color: #e5e5e5; }
html.dark .db-node-latency { color: #888; }
html.dark .db-reset-btn { background: rgba(255,120,0,0.15); color: #ffb366; border-color: rgba(255,255,255,0.1); }
html.dark .db-reset-btn:hover { background: rgba(255,120,0,0.3); }
html.dark .db-reset-info { color: #e5e5e5; }
html.dark .db-pay-title { color: #999; }
html.dark .db-pay-item { background: rgba(255,255,255,0.04); }
html.dark .db-pay-item--selected { border-color: #60a5fa; }
html.dark .db-modal-btn { background: rgba(255,255,255,0.05); color: #e5e5e5; border-color: rgba(255,255,255,0.08); }
html.dark .db-modal-btn:hover { background: rgba(255,255,255,0.08); }
html.dark .db-modal-btn--primary { background: #60a5fa; color: #0a0a0a; }
html.dark .db-action-btn { background: rgba(255,255,255,0.05); color: #e5e5e5; border-color: rgba(255,255,255,0.08); }
html.dark .db-action-btn:hover { background: rgba(255,255,255,0.08); }
html.dark .db-announcement { color: #e5e5e5; }
html.dark .db-announce-dismiss { background: rgba(255,255,255,0.05); color: #e5e5e5; border-color: rgba(255,255,255,0.08); }
html.dark .db-announce-dismiss:hover { background: rgba(255,255,255,0.08); }
html.dark .sub-url-box { background: rgba(255,255,255,0.04); border-color: rgba(255,255,255,0.06); }
html.dark .sub-url { color: #e5e5e5; }
html.dark .sub-action-btn { background: rgba(255,255,255,0.05); color: #e5e5e5; border-color: rgba(255,255,255,0.08); }
html.dark .sub-action-btn:hover { background: rgba(255,255,255,0.08); }
html.dark .sub-qr-wrap { background: #fff; }
html.dark .sub-qr-hint { color: #666; }
</style>
