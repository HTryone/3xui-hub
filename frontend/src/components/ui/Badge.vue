<script setup>
/**
 * Badge.vue — pill status badge.
 * variants: success | warning | danger | info | vless | trojan
 * Each maps to a `--*-muted` background + `--*` text color.
 */
import { computed } from 'vue'

const props = defineProps({
  variant: {
    type: String,
    default: 'info',
    validator: (v) => ['success', 'warning', 'danger', 'info', 'vless', 'trojan'].includes(v),
  },
  dot: { type: Boolean, default: false },
})

const variantClass = computed(() => `ch-badge--${props.variant}`)
</script>

<template>
  <span class="ch-badge" :class="variantClass">
    <span v-if="dot" class="ch-badge-dot" aria-hidden="true"></span>
    <slot />
  </span>
</template>

<style scoped src="@/assets/styles/components/ui/Badge.css"></style>
