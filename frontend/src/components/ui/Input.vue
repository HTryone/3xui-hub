<script setup>
/**
 * Input.vue
 * 尺寸对齐参考项目 shadcn Input：h-9 px-3 py-1 / sm h-8，圆角 --radius-md，
 * 边框用 --border-subtle，focus 走 1px ring（不用发光描边）。
 * mono prop forces JetBrains Mono
 */
import { computed } from 'vue'
import Icon from '@/components/ui/Icon.vue'

const props = defineProps({
  modelValue: { type: [String, Number], default: '' },
  type: { type: String, default: 'text' },
  placeholder: { type: String, default: '' },
  mono: { type: Boolean, default: false },
  disabled: { type: Boolean, default: false },
  // readonly：可选中复制但不可改（如节点密钥这类只供拷贝的值）。
  // 与 disabled 的区别：disabled 会变半透明、且无法选中文本。
  readonly: { type: Boolean, default: false },
  size: { type: String, default: 'md', validator: (v) => ['sm', 'md'].includes(v) },
  valid: { type: Boolean, default: false },
  invalid: { type: Boolean, default: false },
})

const emit = defineEmits(['update:modelValue'])

const inputClass = computed(() => ({
  'ch-input': true,
  'ch-input--mono': props.mono,
  'ch-input--disabled': props.disabled,
  [`ch-input--${props.size}`]: true,
}))

const onInput = (e) => emit('update:modelValue', e.target.value)
</script>

<template>
  <div class="ch-input-wrap" :class="{ 'ch-input-wrap--valid': valid, 'ch-input-wrap--invalid': invalid }">
    <input
      :type="type"
      :class="inputClass"
      :value="modelValue"
      :placeholder="placeholder"
      :disabled="disabled"
      :readonly="readonly"
      @input="onInput"
    />
    <Icon v-if="valid" name="check" :size="16" class="ch-input-check" aria-hidden="true" />
  </div>
</template>

<style scoped src="@/assets/styles/components/ui/Input.css"></style>