<script setup>
/**
 * ListGroup — iOS 列表分组（稿中样式）：分组标题 + 卡片容器 + 脚注。
 */
defineProps({
  label: { type: String, default: '' },
  footnote: { type: String, default: '' },
})
</script>

<template>
  <div class="lg">
    <p v-if="label" class="lg-label">{{ label }}</p>
    <div class="lg-card uu-mat-regular">
      <slot />
    </div>
    <p v-if="footnote" class="lg-footnote">{{ footnote }}</p>
  </div>
</template>

<style scoped src="@/assets/styles/components/user/ListGroup.css"></style>
