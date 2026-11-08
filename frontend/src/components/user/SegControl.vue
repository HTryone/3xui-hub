<script setup>
/**
 * SegControl — iOS 分段控件（稿中样式，滑块 uu-seg-pill 动画）。
 * options: [{ label, value }]，v-model 绑定选中 value。
 */
import { computed } from 'vue'

const props = defineProps({
  options: { type: Array, required: true },
  modelValue: { type: [String, Number], default: '' },
})
const emit = defineEmits(['update:modelValue'])

const idx = computed(() => props.options.findIndex((o) => o.value === props.modelValue))
const pct = computed(() => (props.options.length ? 100 / props.options.length : 100))
const pillStyle = computed(() => ({
  left: `calc(${pct.value * idx.value}% + 2px)`,
  width: `calc(${pct.value}% - 4px)`,
}))
</script>

<template>
  <div class="uu-seg">
    <div class="uu-seg-pill uu-seg-track" :style="pillStyle"></div>
    <button
      v-for="opt in options"
      :key="opt.value"
      type="button"
      class="uu-seg-btn uu-press"
      :class="{ 'uu-seg-btn--active': opt.value === modelValue }"
      @click="emit('update:modelValue', opt.value)"
    >
      {{ opt.label }}
    </button>
  </div>
</template>

<style scoped src="@/assets/styles/components/user/SegControl.css"></style>
