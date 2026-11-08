<script setup>
/**
 * 统计卡：仪表盘的「业务概览」和节点监控的「监控汇总」两排卡片共用。
 *
 * 抽出来的原因：两排卡片原本在 Dashboard.vue / NodeMonitor.vue 里各写了一份
 * 逐字节相同的样式（.ad-stat-* 与 .nm-stat-*），改一处要记得改另一处。
 *
 * 用法：
 *   <StatCard label="用户总数" :value="104" note="注册用户数" />
 *   <StatCard label="服务器" :value="6" :denominator="8" note="在线 / 总数"
 *             health="部分离线" health-state="degraded" />
 *   <StatCard label="覆盖地区" :value="3" suffix="个" note="..." />   ← 单位用 suffix
 *   <StatCard ...><template #note>带链接/条件的长说明</template></StatCard>
 *   <StatCard ...>  ← 默认插槽放迷你曲线之类的附加内容
 *   <StatCard spark ...><svg class="sc-spark-fill">…</svg></StatCard>  ← 曲线铺满整卡背景
 */
import Card from '@/components/ui/Card.vue'

defineProps({
  label: { type: String, required: true },
  value: { type: [String, Number], default: '' },
  /** 数值后缀（如「个」）；与 denominator 互斥使用 */
  suffix: { type: String, default: '' },
  /** 分母，渲染成「 / 8」；null 表示不显示 */
  denominator: { type: [String, Number], default: null },
  note: { type: String, default: '' },
  /** 右上角状态标签文案，空则不显示 */
  health: { type: String, default: '' },
  /** 状态标签配色：healthy | degraded | offline */
  healthState: { type: String, default: '' },
  /** 曲线铺满整卡模式：默认插槽的 SVG 绝对定位铺满卡片背景，文字浮在上层 */
  spark: { type: Boolean, default: false },
})
</script>

<template>
  <Card class="sc" :class="{ 'sc--spark': spark }" :data-health="healthState || undefined">
    <div class="sc-head">
      <p class="sc-label">{{ label }}</p>
      <span v-if="health" class="sc-health">{{ health }}</span>
    </div>

    <p class="sc-value">
      <slot name="value">{{ value }}</slot>
      <span v-if="suffix" class="sc-unit">&nbsp;{{ suffix }}</span>
      <span v-if="denominator !== null" class="sc-unit">&nbsp;/ {{ denominator }}</span>
    </p>

    <p class="sc-note">
      <slot name="note">{{ note }}</slot>
    </p>

    <slot />
  </Card>
</template>

<style scoped src="@/assets/styles/components/ui/StatCard.css"></style>
