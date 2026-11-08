<script setup>
/**
 * ControlHub root shell — router-driven。
 * Toast 全局挂载（任何页面可调 useToast）。
 */
import Toast from '@/components/ui/Toast.vue'
import { useToast } from '@/components/ui/Toast.vue'
import { provide, ref, onMounted } from 'vue'
import { useSiteConfigStore } from '@/stores/siteConfig'
import { version } from '../package.json'

// 让任意后代可 inject('toast')
provide('toast', useToast())

const siteConfig = useSiteConfigStore()
const appReady = ref(false)
onMounted(async () => {
  siteConfig.fetch() // 不 await，后台加载
  // 短暂显示 loading 后立即渲染页面
  setTimeout(() => { appReady.value = true }, 300)
})
</script>

<template>
  <div v-if="!appReady" class="app-loading">
    <div class="app-loading-spinner"></div>
  </div>
  <RouterView v-else />
  <Toast />
  <div class="app-version">v{{ version }}</div>
</template>

<style src="@/assets/styles/base/app.css"></style>
