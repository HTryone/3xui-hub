<script setup>
/**
 * MultiSelect.vue — 多选下拉（视觉与 Select.vue 一致，v-model 为数组）。
 *
 * 为什么单独做：项目里 Select.vue 是单选（v-model 单值），而「批量发信 · 限定套餐」
 * 需要一次勾多个套餐。这里沿用 Select 的触发器 + Teleport 浮层，把选项渲染成
 * 可勾选列表，点选不关闭面板，选完点外部 / Esc 关闭。
 *
 * 用法：
 *   <MultiSelect v-model="arr" :options="opts" placeholder="不限套餐" />
 *   options 元素 { label, value } 或裸字符串/数字。
 */
import { ref, computed, onBeforeUnmount, nextTick } from 'vue'
import { Teleport } from 'vue'
import Icon from './Icon.vue'

const props = defineProps({
  modelValue: { type: Array, default: () => [] },
  options: { type: Array, default: () => [] },
  placeholder: { type: String, default: '不限（全部）' },
  size: { type: String, default: 'md', validator: (v) => ['sm', 'md'].includes(v) },
  disabled: { type: Boolean, default: false },
  wrapperClass: { type: String, default: '' },
})

const emit = defineEmits(['update:modelValue', 'change'])

const open = ref(false)
const trigger = ref(null)
const panel = ref(null)
const panelStyle = ref({})

const normalized = computed(() =>
  props.options.map((o) => {
    if (o !== null && typeof o === 'object') {
      return { label: o.label ?? String(o.value ?? ''), value: o.value }
    }
    return { label: String(o), value: o }
  }),
)

const selectedSet = computed(() => new Set(props.modelValue || []))

const displayText = computed(() => {
  const sel = normalized.value.filter((o) => selectedSet.value.has(o.value))
  if (sel.length === 0) return props.placeholder
  if (sel.length <= 2) return sel.map((o) => o.label).join('、')
  return `已选 ${sel.length} 项`
})

const toggle = () => {
  if (props.disabled) return
  open.value ? close() : show()
}

const show = async () => {
  open.value = true
  await nextTick()
  measure()
  document.addEventListener('mousedown', onOutside, true)
  document.addEventListener('keydown', onKeydown, true)
  window.addEventListener('resize', reposition)
  window.addEventListener('scroll', reposition, true)
}

const close = () => {
  if (!open.value) return
  open.value = false
  document.removeEventListener('mousedown', onOutside, true)
  document.removeEventListener('keydown', onKeydown, true)
  window.removeEventListener('resize', reposition)
  window.removeEventListener('scroll', reposition, true)
}

const reposition = () => { if (open.value) measure() }

const measure = () => {
  const el = trigger.value
  if (!el) return
  const r = el.getBoundingClientRect()
  const margin = 8
  const below = window.innerHeight - r.bottom - margin
  const above = r.top - margin
  const maxH = Math.min(280, Math.max(normalized.value.length * 36 + 44, 140))
  const gap = 4
  let top, maxHeight
  if (below >= Math.min(maxH, needed()) || below >= above) {
    top = r.bottom + gap
    maxHeight = Math.max(Math.min(below, maxH), 80)
  } else {
    top = Math.max(margin, r.top - gap - Math.min(above, maxH))
    maxHeight = Math.max(Math.min(above, maxH), 80)
  }
  const left = Math.max(margin, Math.min(r.left, window.innerWidth - r.width - margin))
  panelStyle.value = {
    top: `${Math.round(top)}px`,
    left: `${Math.round(left)}px`,
    width: `${Math.round(r.width)}px`,
    maxHeight: `${Math.round(maxHeight)}px`,
  }
}

const needed = () => Math.min(normalized.value.length * 36 + 44, 280)

const onOutside = (e) => {
  if (trigger.value?.contains(e.target) || panel.value?.contains(e.target)) return
  close()
}

const onKeydown = (e) => { if (e.key === 'Escape') { close(); trigger.value?.focus() } }

// 点选不关闭面板：多选要能连续勾
const toggleValue = (opt) => {
  const cur = new Set(props.modelValue || [])
  cur.has(opt.value) ? cur.delete(opt.value) : cur.add(opt.value)
  const arr = normalized.value.map((o) => o.value).filter((v) => cur.has(v))
  emit('update:modelValue', arr)
  emit('change', arr)
}

const clearAll = () => {
  emit('update:modelValue', [])
  emit('change', [])
}

onBeforeUnmount(close)
</script>

<template>
  <div class="ch-select ch-multi" :class="[`ch-select--${size}`, wrapperClass, { 'is-open': open, 'is-disabled': disabled, 'is-placeholder': !modelValue?.length }]">
    <button
      ref="trigger"
      type="button"
      class="ch-select-trigger"
      :disabled="disabled"
      :aria-expanded="open"
      aria-haspopup="listbox"
      @click="toggle"
    >
      <span class="ch-select-value">{{ displayText }}</span>
      <Icon name="chevron-down" :size="16" class="ch-select-caret" />
    </button>

    <Teleport to="body">
      <Transition name="ch-select-pop">
        <div v-if="open" ref="panel" class="ch-select-panel" :style="panelStyle" role="listbox" aria-multiselectable="true">
          <button
            v-for="opt in normalized"
            :key="String(opt.value)"
            type="button"
            class="ch-select-option"
            :class="{ 'is-selected': selectedSet.has(opt.value) }"
            role="option"
            :aria-selected="selectedSet.has(opt.value)"
            @click="toggleValue(opt)"
          >
            <span class="ch-multi-box" :class="{ 'is-on': selectedSet.has(opt.value) }">
              <Icon v-if="selectedSet.has(opt.value)" name="check" :size="12" />
            </span>
            <span class="ch-multi-label">{{ opt.label }}</span>
          </button>
          <div class="ch-multi-foot">
            <button type="button" class="ch-multi-clear" @click="clearAll">清空（不限）</button>
          </div>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>

<style scoped src="@/assets/styles/components/ui/MultiSelect.css"></style>
