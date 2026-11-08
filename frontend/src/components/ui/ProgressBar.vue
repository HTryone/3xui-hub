<script setup>
/**
 * ProgressBar.vue
 * - props percent (0-100)
 * - track: var(--bg-hover); fill: var(--accent); radius full
 */
import { computed } from 'vue'

const props = defineProps({
  percent: { type: Number, default: 0 },
  label: { type: String, default: '' },
})

const clamped = computed(() => Math.max(0, Math.min(100, props.percent)))
const widthStyle = computed(() => ({ width: `${clamped.value}%` }))
</script>

<template>
  <div class="ch-progress" role="progressbar" :aria-valuenow="clamped" aria-valuemin="0" aria-valuemax="100">
    <div class="ch-progress-track">
      <div class="ch-progress-fill" :style="widthStyle"></div>
    </div>
    <span v-if="label" class="ch-progress-label">{{ label }}</span>
  </div>
</template>

<style scoped src="@/assets/styles/components/ui/ProgressBar.css"></style>
