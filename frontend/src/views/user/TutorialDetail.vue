<script setup>
/**
 * TutorialDetail — 用户端教程详情
 */
import { ref, onMounted, computed } from 'vue'
import { useRoute } from 'vue-router'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import { userApi } from '@/api/user'

const route = useRoute()
const tutorial = ref(null)
const loading = ref(true)

const renderedContent = computed(() => {
  if (!tutorial.value?.content) return ''
  return DOMPurify.sanitize(marked.parse(tutorial.value.content))
})

// 返回统一由外壳表头（UserLayout）承担，此处不再重复放按钮

onMounted(async () => {
  try {
    tutorial.value = await userApi.tutorial(route.params.id)
  } catch (e) {
    tutorial.value = null
  } finally {
    loading.value = false
  }
})
</script>

<template>
  <div class="td">
    <div v-if="loading" class="td-loading">加载中...</div>

    <div v-else-if="tutorial" class="td-content">
      <h1 class="td-title">{{ tutorial.title }}</h1>
      <div class="td-meta">{{ new Date(tutorial.created_at).toLocaleDateString() }}</div>
      <div class="td-body" v-html="renderedContent"></div>
    </div>

    <div v-else class="td-empty">
      <p>教程不存在或已下架</p>
    </div>
  </div>
</template>

<style scoped src="@/assets/styles/pages/user/TutorialDetail.css"></style>
