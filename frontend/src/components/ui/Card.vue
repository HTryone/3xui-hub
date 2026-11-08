<script setup>
/**
 * Card.vue
 * 排版对齐参考项目 shadcn Card：圆角 --radius-xl (12px)、1px 边框、内边距 24px。
 *
 * 质感处理：默认 --shadow-md 让卡片从页面底色上浮起来；interactive 时
 * 阴影加深 + 上浮 1px + 顶部高光边，鼠标移上去有明确的「可点」反馈。
 * 用法：<Card interactive> 或 <Card interactive @click="...">
 */
defineProps({
  padding: { type: Boolean, default: true },
  // 可交互卡片：悬浮加深阴影并轻微上浮
  interactive: { type: Boolean, default: false },
})
</script>

<template>
  <div
    class="ch-card"
    :class="{
      'ch-card--flush': !padding,
      'ch-card--interactive': interactive,
    }"
  >
    <!-- 具名 header：卡片标题区。此前组件未实现该插槽，用 #header 的页面标题不渲染 -->
    <div v-if="$slots.header" class="ch-card-header">
      <slot name="header" />
    </div>
    <slot />
  </div>
</template>

<style scoped src="@/assets/styles/components/ui/Card.css"></style>