<script setup>
/**
 * 公开主页 — Apple 风格 + 世界地图节点动画
 */
import { ref, computed, onMounted, onUnmounted, nextTick, watch } from 'vue'
import { useRouter } from 'vue-router'
import { gsap } from 'gsap'
import { ScrollTrigger } from 'gsap/ScrollTrigger'
import Button from '@/components/ui/Button.vue'
import Icon from '@/components/ui/Icon.vue'
import ThemeToggle from '@/components/ui/ThemeToggle.vue'
import { useSiteConfigStore } from '@/stores/siteConfig'
import CustomHome from '@/components/CustomHome.vue'
import { useAuthStore } from '@/stores/auth'
import { userApi } from '@/api/user'

gsap.registerPlugin(ScrollTrigger)

const router = useRouter()
const auth = useAuthStore()
const siteConfig = useSiteConfigStore()
const plans = ref([])
const planTab = ref('period')
const container = ref(null)
let ctx
let planTrigger = null

const filteredPlans = computed(() => plans.value.filter(p => p.type === planTab.value))

// 用户请求减少动效时跳过所有 GSAP 动画，直接呈现最终状态
const reduceMotion = () =>
  typeof window !== 'undefined' &&
  window.matchMedia?.('(prefers-reduced-motion: reduce)').matches

// 套餐卡入场：元素必须已渲染才能建立，否则 gsap.set/batch 作用在空集合上（静默失效）
const setupPlanAnim = (visible) => {
  planTrigger?.kill()
  planTrigger = null
  if (!visible || !container.value) return

  const els = container.value.querySelectorAll('.lp-plan')
  if (!els.length) return
  if (reduceMotion()) return

  // 卡片已在视口内就直接淡入，避免切换 Tab 后停留在隐藏态
  const inView = els[0].getBoundingClientRect().top < window.innerHeight
  if (inView) {
    gsap.fromTo(els, { autoAlpha: 0, y: 24 }, {
      autoAlpha: 1, y: 0, duration: 0.55, ease: 'power3.out', stagger: 0.1, overwrite: 'auto',
    })
    return
  }

  gsap.set(els, { autoAlpha: 0, y: 60 })
  planTrigger = ScrollTrigger.batch(els, {
    onEnter: (batch) =>
      gsap.to(batch, {
        autoAlpha: 1, y: 0, duration: 0.8, ease: 'power3.out', stagger: 0.15, overwrite: 'auto',
      }),
    start: 'top 85%',
    once: true,
  })
}

const goPlan = () => { auth.token ? router.push('/dashboard/shop') : router.push('/register') }

const fmtBytes = (b) => {
  if (!b || b < 0 || !Number.isFinite(b)) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB', 'EB']
  const raw = Math.floor(Math.log(b) / Math.log(1024))
  const i = Math.max(0, Math.min(raw, u.length - 1))
  return `${(b / Math.pow(1024, i)).toFixed(i ? 2 : 0)} ${u[i]}`
}

// 节点数据
const nodes = [
  { name: 'Tokyo', x: 82, y: 36 },
  { name: 'Seoul', x: 78, y: 34 },
  { name: 'Hong Kong', x: 74, y: 46 },
  { name: 'Singapore', x: 72, y: 56 },
  { name: 'Frankfurt', x: 50, y: 30 },
  { name: 'London', x: 47, y: 28 },
  { name: 'San José', x: 15, y: 36 },
  { name: 'Mumbai', x: 64, y: 46 },
]

onMounted(async () => {
  // 公开首页不走 UserLayout，必须自己确保站点配置已就位，
  // 否则 siteConfig.config 仍是初始空值 → 自定义首页内容读不到 → 整页空白。
  // App.vue 里 fetch() 不 await，只靠 300ms 定时渲染，慢一点就会踩空。
  if (!siteConfig.loaded) {
    try { await siteConfig.fetch() } catch (e) { /* 拉不到就走默认首页 */ }
  }
  try { plans.value = await userApi.plans() } catch (e) {}
  // 等 Vue 把套餐卡片渲染进 DOM，再建立动画（否则元素不存在，动画静默失效）
  await nextTick()
  if (!container.value) return

  // 减少动效：只保留必要的 GSAP 行为，跳过全部入场/滚动动画
  if (reduceMotion()) {
    gsap.set(container.value.querySelectorAll('.lp-plan'), { clearProps: 'all' })
    return
  }

  ctx = gsap.context(() => {
    // === 响应式动画参数 ===
    const mm = gsap.matchMedia()
    mm.add('(max-width: 768px)', () => {
      // 移动端：缩短距离，减少位移
      ScrollTrigger.config({ ignoreMobileResize: true })
    })

    // === Nav 入场 ===
    gsap.fromTo('.lp-nav-inner', { y: -20, autoAlpha: 0 }, { y: 0, autoAlpha: 1, duration: 0.8, ease: 'power3.out', delay: 0.1 })

    // === Hero 入场 ===
    gsap.fromTo('.lp-hero-title', { y: 80, autoAlpha: 0 }, { y: 0, autoAlpha: 1, duration: 1.2, ease: 'power3.out', delay: 0.2 })
    if (document.querySelector('.lp-hero-sub')) {
      gsap.fromTo('.lp-hero-sub', { y: 40, autoAlpha: 0 }, { y: 0, autoAlpha: 1, duration: 0.8, ease: 'power3.out', delay: 0.5 })
    }
    gsap.fromTo('.lp-hero-actions', { y: 40, autoAlpha: 0 }, { y: 0, autoAlpha: 1, duration: 0.8, ease: 'power3.out', delay: 0.7 })
    gsap.fromTo('.lp-map-wrap', { scale: 0.9, autoAlpha: 0 }, { scale: 1, autoAlpha: 1, duration: 1.2, ease: 'power3.out', delay: 0.4 })

    // Hero 统计数据交错入场
    gsap.fromTo('.lp-hero-stat', { y: 30, autoAlpha: 0 }, { y: 0, autoAlpha: 1, duration: 0.6, ease: 'power3.out', stagger: 0.15, delay: 0.9 })

    // === 节点脉冲 ===
    gsap.utils.toArray('.lp-node-pulse').forEach((el, i) => {
      gsap.to(el, { scale: 3, opacity: 0, duration: 2, ease: 'power1.out', repeat: -1, delay: i * 0.3 })
    })

    // === 连接线流动 ===
    gsap.utils.toArray('.lp-map-line').forEach((line) => {
      const length = line.getTotalLength()
      gsap.set(line, { strokeDasharray: length, strokeDashoffset: length })
      gsap.to(line, { strokeDashoffset: 0, duration: 2, ease: 'power2.inOut', repeat: -1, repeatDelay: 1 })
    })

    // === Hero Pin ===
    gsap.to('.lp-hero-content', { scale: 0.85, y: -100, autoAlpha: 0, ease: 'power2.inOut', scrollTrigger: { trigger: '.lp-hero', start: 'top top', end: '+=500', scrub: true } })

    // === 功能区入场 ===
    // 大标题
    gsap.utils.toArray('.lp-feature-heading').forEach((el) => {
      gsap.fromTo(el, { x: -60, autoAlpha: 0 }, { x: 0, autoAlpha: 1, duration: 1, ease: 'power3.out', scrollTrigger: { trigger: el, start: 'top 85%', once: true } })
    })
    // 描述文字
    gsap.utils.toArray('.lp-feature-desc').forEach((el) => {
      gsap.fromTo(el, { y: 20, autoAlpha: 0 }, { y: 0, autoAlpha: 1, duration: 0.8, ease: 'power3.out', delay: 0.2, scrollTrigger: { trigger: el, start: 'top 85%', once: true } })
    })
    // 列表项交错
    gsap.utils.toArray('.lp-feature-list').forEach((list) => {
      const items = list.querySelectorAll('li')
      gsap.set(items, { autoAlpha: 0, x: -20 })
      ScrollTrigger.create({
        trigger: list,
        start: 'top 85%',
        once: true,
        onEnter: () => gsap.to(items, { autoAlpha: 1, x: 0, duration: 0.5, ease: 'power3.out', stagger: 0.1 })
      })
    })
    // 统计卡片
    gsap.utils.toArray('.lp-feature-card-outer').forEach((el) => {
      gsap.fromTo(el, { scale: 0.9, autoAlpha: 0 }, { scale: 1, autoAlpha: 1, duration: 0.8, ease: 'power3.out', scrollTrigger: { trigger: el, start: 'top 85%', once: true } })
    })
    // 功能区数字跳动
    gsap.utils.toArray('.lp-feature-stat-num').forEach((el) => {
      const text = el.textContent
      const numMatch = text.match(/(\d+)/)
      if (!numMatch) return
      const target = parseInt(numMatch[1], 10)
      const prefix = text.split(numMatch[1])[0]
      const suffix = text.split(numMatch[1])[1]
      const obj = { value: 0 }
      gsap.to(obj, { value: target, duration: 2, ease: 'power2.out', snap: { value: 1 },
        scrollTrigger: { trigger: el, start: 'top 85%', once: true },
        onUpdate() { el.textContent = prefix + Math.round(obj.value).toLocaleString() + suffix }
      })
    })
    // 分隔线展开
    gsap.utils.toArray('.lp-feature-divider').forEach((el) => {
      gsap.fromTo(el, { scaleX: 0, transformOrigin: 'left center' }, { scaleX: 1, duration: 1, ease: 'power2.inOut', scrollTrigger: { trigger: el, start: 'top 90%', once: true } })
    })

    // === Section 标题 ===
    gsap.utils.toArray('.lp-section-title, .lp-eyebrow:not(.lp-eyebrow-hero)').forEach((el) => {
      gsap.fromTo(el, { y: 30, autoAlpha: 0 }, { y: 0, autoAlpha: 1, duration: 0.8, ease: 'power3.out', scrollTrigger: { trigger: el, start: 'top 88%', once: true } })
    })

    // === 套餐 Tab ===
    if (document.querySelector('.lp-plan-tabs')) {
      gsap.fromTo('.lp-plan-tabs', { y: 20, autoAlpha: 0 }, { y: 0, autoAlpha: 1, duration: 0.6, ease: 'power3.out', scrollTrigger: { trigger: '.lp-plan-tabs', start: 'top 88%', once: true } })
    }

    // === 套餐卡片 ===
    // 注意：卡片由 async 数据渲染，setupPlanAnim 只在元素真实存在时才建立，
    // 且切换 Tab 后要重新调用（否则新卡片既没有入场也没有隐藏）
    setupPlanAnim(plans.value.length > 0)

    // === Hero 数字跳动 ===
    document.querySelectorAll('.lp-hero-stat .lp-stat-num').forEach((el) => {
      const text = el.getAttribute('data-target') || ''
      const numMatch = text.match(/(\d+)/)
      if (!numMatch) return
      const target = parseInt(numMatch[1], 10)
      const prefix = text.split(numMatch[1])[0]
      const suffix = text.split(numMatch[1])[1]
      const obj = { value: 0 }
      gsap.to(obj, { value: target, duration: 2, ease: 'power2.out', snap: { value: 1 },
        scrollTrigger: { trigger: el.closest('.lp-hero-stat') || el, start: 'top 80%', once: true },
        onUpdate() { el.textContent = prefix + Math.round(obj.value).toLocaleString() + suffix }
      })
    })

    // === CTA ===
    gsap.fromTo('.lp-cta-inner', { autoAlpha: 0, scale: 0.85, y: 40 }, { autoAlpha: 1, scale: 1, y: 0, duration: 1, ease: 'power3.out', scrollTrigger: { trigger: '.lp-cta', start: 'top 80%', once: true } })

    // === Footer ===
    gsap.fromTo('.lp-footer', { autoAlpha: 0 }, { autoAlpha: 1, duration: 0.6, scrollTrigger: { trigger: '.lp-footer', start: 'top 95%', once: true } })

  }, container.value)
})

// 切换套餐 Tab：卡片重新渲染后再放动画，避免新卡片生硬出现
watch(planTab, async () => {
  if (!container.value || reduceMotion()) return
  await nextTick()
  setupPlanAnim(filteredPlans.value.length > 0)
})

onUnmounted(() => {
  planTrigger?.kill()
  ctx?.revert()
})

// ===== 自定义公开首页 =====
// 管理员在侧栏「主页」里用 HTML+CSS 自己搭公开首页（见 components/CustomHome.vue）。
// 为空 = 走下面原有的默认首页，所以老面板升级后这里完全不影响原有行为。
const customHtml = computed(() => String(siteConfig.config?.home_custom_html || '').trim())
const customCss = computed(() => siteConfig.config?.home_custom_css || '')
const hasCustomHome = computed(() => customHtml.value !== '')
</script>

<template>
  <!-- 管理员自定义首页：有内容就整页替换，没内容走下面默认首页 -->
  <CustomHome v-if="hasCustomHome" :html="customHtml" :css="customCss" />
  <div v-else ref="container" class="lp">
    <!-- Nav -->
    <nav class="lp-nav">
      <div class="lp-nav-inner">
        <div class="lp-nav-brand">
          <span class="lp-nav-logo">
            <img v-if="siteConfig.logo" :src="siteConfig.logo" alt="" class="lp-nav-logo-img" />
            <Icon v-else name="shield" :size="20" />
          </span>
          <span class="lp-nav-name">{{ siteConfig.title }}</span>
        </div>
        <div class="lp-nav-links">
          <a href="#features">功能</a>
          <a href="#pricing">套餐</a>
        </div>
        <Button variant="primary" size="sm" class="lp-nav-btn" @click="router.push('/login')">登录</Button>
      </div>
    </nav>

    <!-- 主题切换 - 右上角固定 -->
    <div class="lp-theme-fixed">
      <ThemeToggle />
    </div>

    <!-- Hero + Map -->
    <section class="lp-hero">
      <div class="lp-hero-content">
        <span class="lp-eyebrow lp-eyebrow-hero">安全 · 高速 · 稳定</span>
        <h1 class="lp-hero-title">
          {{ siteConfig.title }}<span class="lp-hero-dot">.</span>
        </h1>
        <p v-if="siteConfig.subtitle" class="lp-hero-sub">{{ siteConfig.subtitle }}</p>
        <div class="lp-hero-actions">
          <button class="lp-btn lp-btn--primary" @click="goPlan">
            立即开始
            <span class="lp-btn-icon"><Icon name="chevron-right" :size="16" /></span>
          </button>
          <button class="lp-btn lp-btn--ghost" @click="router.push('/login')">登录账户</button>
        </div>
        <div class="lp-hero-stats">
          <div class="lp-hero-stat">
            <span class="lp-stat-num" data-target="30+">0+</span>
            <span class="lp-stat-label">全球节点</span>
          </div>
          <div class="lp-hero-stat">
            <span class="lp-stat-num" data-target="5Gbps">0Gbps</span>
            <span class="lp-stat-label">单线带宽</span>
          </div>
          <div class="lp-hero-stat">
            <span class="lp-stat-num" data-target="99.9%">0%</span>
            <span class="lp-stat-label">在线率</span>
          </div>
        </div>
      </div>

      <!-- World Map -->
      <div class="lp-map-wrap">
        <img src="/images/world.svg" alt="" class="lp-map-img" />
        <svg class="lp-map-overlay" viewBox="0 0 100 100" preserveAspectRatio="none">
          <!-- 连接线 -->
          <line class="lp-map-line" x1="15" y1="36" x2="50" y2="30" />
          <line class="lp-map-line" x1="50" y1="30" x2="74" y2="46" />
          <line class="lp-map-line" x1="74" y1="46" x2="82" y2="36" />
          <line class="lp-map-line" x1="47" y1="28" x2="64" y2="46" />
          <line class="lp-map-line" x1="64" y1="46" x2="72" y2="56" />
          <line class="lp-map-line" x1="78" y1="34" x2="82" y2="36" />
          <line class="lp-map-line" x1="15" y1="36" x2="47" y2="28" />
        </svg>
        <!-- 节点 -->
        <div v-for="node in nodes" :key="node.name" class="lp-node" :style="{ left: node.x + '%', top: node.y + '%' }">
          <span class="lp-node-pulse"></span>
          <span class="lp-node-dot"></span>
          <span class="lp-node-label">{{ node.name }}</span>
        </div>
      </div>
    </section>

    <!-- Features -->
    <section id="features" class="lp-section">
      <span class="lp-eyebrow">核心优势</span>
      <h2 class="lp-section-title">大概你需要的就这些</h2>

      <div class="lp-feature-row">
        <div class="lp-feature-text">
          <h3 class="lp-feature-heading">无界<span class="lp-feature-dot">.</span></h3>
          <p class="lp-feature-desc">直连全球节点，无地域限制，低延迟高速访问</p>
          <ul class="lp-feature-list">
            <li>覆盖全球 30+ 节点</li>
            <li>智能路由，自动选择最优线路</li>
            <li>支持 IPLC 专线，延迟更低</li>
          </ul>
        </div>
        <div class="lp-feature-visual">
          <div class="lp-feature-card-outer">
            <div class="lp-feature-card-inner">
              <span class="lp-feature-stat-num">30+</span>
              <span class="lp-feature-stat-label">全球节点</span>
            </div>
          </div>
        </div>
      </div>

      <div class="lp-feature-divider"></div>

      <div class="lp-feature-row lp-feature-row--reverse">
        <div class="lp-feature-text">
          <h3 class="lp-feature-heading">加密<span class="lp-feature-dot">.</span></h3>
          <p class="lp-feature-desc">我们不追踪、不收集、不分享你的隐私数据</p>
          <ul class="lp-feature-list">
            <li>零日志策略，隐私有保障</li>
            <li>DNS 泄露防护</li>
            <li>先进加密协议，数据安全</li>
          </ul>
        </div>
        <div class="lp-feature-visual">
          <div class="lp-feature-card-outer">
            <div class="lp-feature-card-inner">
              <span class="lp-feature-stat-num">零</span>
              <span class="lp-feature-stat-label">日志策略</span>
            </div>
          </div>
        </div>
      </div>

      <div class="lp-feature-divider"></div>

      <div class="lp-feature-row">
        <div class="lp-feature-text">
          <h3 class="lp-feature-heading">优化<span class="lp-feature-dot">.</span></h3>
          <p class="lp-feature-desc">99.9% 在线率，7×24 运维保障，多设备同时在线</p>
          <ul class="lp-feature-list">
            <li>99.9% 服务可用性</li>
            <li>不限速，全节点可用</li>
            <li>手机、电脑、平板多端同时使用</li>
          </ul>
        </div>
        <div class="lp-feature-visual">
          <div class="lp-feature-card-outer">
            <div class="lp-feature-card-inner">
              <span class="lp-feature-stat-num">99.9%</span>
              <span class="lp-feature-stat-label">在线率</span>
            </div>
          </div>
        </div>
      </div>
    </section>

    <!-- Pricing -->
    <section id="pricing" class="lp-section lp-section--alt">
      <span class="lp-eyebrow">灵活方案</span>
      <h2 class="lp-section-title">选择适合你的套餐</h2>
      <div class="lp-plan-tabs">
        <button :class="['lp-plan-tab', { active: planTab === 'total' }]" @click="planTab = 'total'">总量套餐</button>
        <button :class="['lp-plan-tab', { active: planTab === 'period' }]" @click="planTab = 'period'">周期套餐</button>
      </div>
      <div class="lp-plans">
        <div v-for="(plan, idx) in filteredPlans" :key="plan.id" class="lp-plan" :class="{ 'lp-plan--highlight': idx === 1 }">
          <div class="lp-plan-outer">
            <div class="lp-plan-inner">
              <h3 class="lp-plan-name">{{ plan.name }}</h3>
              <div class="lp-plan-price">
                <span class="lp-plan-currency">¥</span>
                <span class="lp-plan-amount">{{ plan.price || 0 }}</span>
                <span class="lp-plan-unit">{{ plan.type === 'period' ? (plan.months === 1 ? '/月' : plan.months === 3 ? '/季' : plan.months === 12 ? '/年' : '/' + plan.months + '个月') : '' }}</span>
              </div>
              <p v-if="plan.type === 'period' && plan.months > 1" class="lp-plan-equiv">{{ (plan.price / plan.months).toFixed(1) }}元/月</p>
              <ul class="lp-plan-features">
                <template v-if="!plan.include_local && plan.third_party_sub_ids?.length">
                  <li>{{ plan.months }} 个月有效期</li>
                  <li>高速线路可用</li>
                  <li>不限速</li>
                </template>
                <template v-else-if="plan.type === 'period'">
                  <li>{{ plan.months }} 个月有效期</li>
                  <li>每月 {{ fmtBytes(plan.monthly_traffic) }}</li>
                  <li>流量按月重置</li>
                  <li>全节点可用</li>
                  <li>不限速</li>
                </template>
                <template v-else>
                  <li>总量 {{ fmtBytes(plan.total_traffic) }}</li>
                  <li>永久有效</li>
                  <li>用完为止</li>
                  <li>全节点可用</li>
                  <li>不限速</li>
                </template>
              </ul>
              <button class="lp-plan-btn" :class="idx === 1 ? 'lp-plan-btn--dark' : 'lp-plan-btn--light'" @click="goPlan">
                选择 {{ plan.name }}
              </button>
            </div>
          </div>
        </div>
      </div>
      <p v-if="!filteredPlans.length" class="lp-empty">暂无可用套餐</p>
    </section>

    <!-- CTA -->
    <section class="lp-cta">
      <div class="lp-cta-inner">
        <h2>准备好开始了吗？</h2>
        <p>注册即可体验高速稳定的网络服务</p>
        <button class="lp-btn lp-btn--primary" @click="goPlan">
          免费注册
          <span class="lp-btn-icon"><Icon name="chevron-right" :size="16" /></span>
        </button>
      </div>
    </section>

    <footer class="lp-footer">
      <p>{{ siteConfig.title }} &copy; {{ new Date().getFullYear() }}</p>
    </footer>
  </div>
</template>

<style scoped src="@/assets/styles/pages/user/Home.css"></style>
