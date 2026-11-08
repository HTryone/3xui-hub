<script setup>
/**
 * CopyButton.vue — 复制变勾（ui-motion #40）。
 * 点击复制 → 图标变绿色对勾弹入 → 约 1.5s 后恢复复制图标。
 * 只动 transform/opacity；失败时静默降级（老浏览器 execCommand 兜底）。
 */
import { ref } from 'vue'
import Icon from '@/components/ui/Icon.vue'

const props = defineProps({
  text: { type: String, default: '' },
  // 动态/异步取文本：复制前调用，返回要复制的字符串（如异步拿的出站链接）
  getText: { type: Function, default: null },
  label: { type: String, default: '' },
})

const copied = ref(false)
let timer = null

async function copy() {
  let t = props.text
  if (props.getText) {
    try {
      t = await props.getText()
    } catch {
      return
    }
  }
  if (!t) return
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(t)
    } else {
      fallbackCopy(t)
    }
  } catch {
    fallbackCopy(t)
  }
  copied.value = true
  clearTimeout(timer)
  timer = setTimeout(() => {
    copied.value = false
  }, 1500)
}

function fallbackCopy(text) {
  const ta = document.createElement('textarea')
  ta.value = text
  ta.style.position = 'fixed'
  ta.style.opacity = '0'
  document.body.appendChild(ta)
  ta.select()
  try {
    document.execCommand('copy')
  } catch {
    /* 忽略：极老浏览器 */
  }
  document.body.removeChild(ta)
}
</script>

<template>
  <button
    type="button"
    class="copy-btn"
    :class="{ 'is-copied': copied }"
    :title="copied ? '已复制' : '复制'"
    :aria-label="copied ? '已复制' : '复制'"
    @click="copy"
  >
    <Icon :name="copied ? 'check' : 'copy'" :size="16" />
    <span v-if="label" class="copy-label">{{ copied ? '已复制' : label }}</span>
  </button>
</template>

<style scoped src="@/assets/styles/components/ui/CopyButton.css"></style>
