<script setup>
/**
 * Skeleton.vue — 骨架屏扫光（ui-motion #15）。
 * 列表加载时用「灰底 + 高光斜扫」替代白屏转圈，结构已知所以提前占位。
 * 只动 transform（扫光），不触发重排；prefers-reduced-motion 时降级为静态。
 */
defineProps({
  rows: { type: Number, default: 5 },
  height: { type: String, default: '40px' },
  animate: { type: Boolean, default: true },
})
</script>

<template>
  <div class="skel" :class="{ 'skel--static': !animate }" aria-hidden="true">
    <div v-for="i in rows" :key="i" class="skel-row" :style="{ height }">
      <span class="skel-bar"></span>
    </div>
  </div>
</template>

<style scoped src="@/assets/styles/components/ui/Skeleton.css"></style>
