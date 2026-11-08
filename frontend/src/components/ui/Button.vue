<script setup>
/**
 * Button.vue
 * props: variant (primary|secondary|danger|ghost) / size (sm|md|lg) / loading / disabled
 * 尺寸对齐参考项目 shadcn Button：h-9 / sm h-8 / lg h-10，圆角 --radius-md，
 * 字重 medium，focus 走 1px ring 而非发光。颜色全部走 CSS 变量，无硬编码色值。
 */
import Icon from '@/components/ui/Icon.vue'

const props = defineProps({
  variant: { type: String, default: 'primary', validator: (v) => ['primary', 'secondary', 'danger', 'ghost'].includes(v) },
  size: { type: String, default: 'md', validator: (v) => ['sm', 'md', 'lg'].includes(v) },
  loading: { type: Boolean, default: false },
  success: { type: Boolean, default: false },
  disabled: { type: Boolean, default: false },
  type: { type: String, default: 'button' },
})

const sizeClass = {
  sm: 'btn-sm',
  md: 'btn-md',
  lg: 'btn-lg',
}[props.size]
</script>

<template>
  <button
    :type="type"
    class="ch-btn"
    :class="[`btn-${variant}`, sizeClass, { 'is-loading': loading, 'is-success': success, 'is-disabled': disabled }]"
    :disabled="disabled || loading || success"
  >
    <span v-if="loading" class="ch-btn-spinner" aria-hidden="true"></span>
    <Icon v-else-if="success" name="check" :size="16" class="ch-btn-check" aria-hidden="true" />
    <slot />
  </button>
</template>

<style scoped src="@/assets/styles/components/ui/Button.css"></style>