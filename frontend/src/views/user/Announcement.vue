<script setup>
/**
 * Announcement — 系统公告全屏页。
 * 内容来自 siteConfig.config.announcement，沿用既有的纯文本 + pre-wrap 渲染方式。
 */
import { computed } from 'vue'
import { useSiteConfigStore } from '@/stores/siteConfig'

const siteConfig = useSiteConfigStore()
const content = computed(() => (siteConfig.config.announcement || '').trim())
</script>

<template>
  <div class="ann uu-slide-up">
    <!-- 内容卡片 -->
    <section class="ann-card uu-mat-regular">
      <div class="ann-box">
        <p v-if="content" class="ann-content">{{ content }}</p>
        <p v-else class="ann-empty">暂无公告</p>
      </div>
    </section>
  </div>
</template>

<style scoped src="@/assets/styles/pages/user/Announcement.css"></style>
