<script setup>
/**
 * ExpandText.vue — 长文本默认截断，点开看全文。
 *
 * 审计日志的 error、任务运行的失败原因都是后端原文（可能上千字）：整段铺在表格里
 * 把行高撑爆，截断了又看不到全貌。默认只显示 limit 个字，超长才给「展开 / 收起」，
 * 点击文字本身也能切换（表格里不用精准点小按钮）。
 */
import { computed, ref } from 'vue'

const props = defineProps({
  text: { type: [String, Number], default: '' },
  // limit：折叠时保留的字数（「约 80 字」的口径）
  limit: { type: Number, default: 80 },
  // danger：整段用报错色（失败原因 / 漂移说明）
  danger: { type: Boolean, default: false },
})

const open = ref(false)
const raw = computed(() => (props.text == null ? '' : String(props.text)))
const truncated = computed(() => raw.value.length > props.limit)
const shown = computed(() =>
  !truncated.value || open.value ? raw.value : `${raw.value.slice(0, props.limit)}…`,
)
const toggle = () => {
  if (truncated.value) open.value = !open.value
}
</script>

<template>
  <span class="ex" :class="{ 'ex--danger': danger, 'ex--open': truncated && open }">
    <span class="ex-text" :class="{ 'ex-text--clickable': truncated }" @click="toggle">{{ shown }}</span>
    <button
      v-if="truncated"
      type="button"
      class="ex-btn"
      :aria-expanded="open ? 'true' : 'false'"
      @click="toggle"
    >{{ open ? '收起' : '展开' }}</button>
  </span>
</template>

<style scoped src="@/assets/styles/components/ui/ExpandText.css"></style>
