<script setup>
/**
 * 主题切换按钮：暗色显示太阳(点击转亮)，亮色显示月亮(点击转暗)。
 */
import Icon from '@/components/ui/Icon.vue'
import { useThemeStore } from '@/stores/theme'

const theme = useThemeStore()
</script>

<template>
  <button
    class="theme-toggle"
    type="button"
    :aria-label="theme.isDark ? '切换到亮色主题' : '切换到暗色主题'"
    :title="theme.isDark ? '亮色' : '暗色'"
    @click="theme.toggle()"
  >
    <Icon :name="theme.isDark ? 'sun' : 'moon'" :size="16" />
  </button>
</template>

<style scoped src="@/assets/styles/components/ui/ThemeToggle.css"></style>
