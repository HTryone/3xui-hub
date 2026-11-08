<script setup>
/**
 * 用户登录 — 精致优雅风格
 */
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Input from '@/components/ui/Input.vue'
import Icon from '@/components/ui/Icon.vue'
import ThemeToggle from '@/components/ui/ThemeToggle.vue'
import { useAuthStore } from '@/stores/auth'
import { useSiteConfigStore } from '@/stores/siteConfig'
import { useToast } from '@/components/ui/Toast.vue'

const auth = useAuthStore()
const siteConfig = useSiteConfigStore()
const route = useRoute()
const router = useRouter()
const toast = useToast()

const mode = ref('email')
const email = ref('')
const password = ref('')
const token = ref('')
const loading = ref(false)

const onEmailLogin = async () => {
  if (!email.value.trim() || !password.value) {
    toast.error('请输入邮箱和密码')
    return
  }
  loading.value = true
  try {
    await auth.loginEmail(email.value.trim(), password.value)
    await auth.fetchMe()
    toast.success('登录成功')
    router.push(route.query.redirect || '/dashboard')
  } catch (e) {
    toast.error(e.message || '登录失败')
  } finally {
    loading.value = false
  }
}

const onTokenLogin = async () => {
  if (!token.value.trim()) {
    toast.error('请输入 Token')
    return
  }
  loading.value = true
  try {
    await auth.login(token.value.trim())
    await auth.fetchMe()
    toast.success('登录成功')
    router.push(route.query.redirect || '/dashboard')
  } catch (e) {
    toast.error(e.message || '登录失败')
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="login-page">
    <div class="login-bg">
      <div class="login-bg-circle login-bg-circle--1"></div>
      <div class="login-bg-circle login-bg-circle--2"></div>
    </div>

    <div class="login-top">
      <router-link to="/" class="login-back">
        <Icon name="chevron-left" :size="16" />
        <span>返回首页</span>
      </router-link>
      <ThemeToggle />
    </div>

    <div class="login-card">
      <div class="login-head">
        <span class="login-logo">
          <img v-if="siteConfig.logo" :src="siteConfig.logo" alt="" class="login-logo-img" />
          <Icon v-else name="shield" :size="24" />
        </span>
        <h1 class="login-title">{{ siteConfig.title }}</h1>
        <p class="login-sub">登录你的账户</p>
      </div>

      <form v-if="mode === 'email'" class="login-form" @submit.prevent="onEmailLogin">
        <label class="login-field">
          <span>邮箱</span>
          <Input v-model="email" type="email" placeholder="your@email.com" :disabled="loading" />
        </label>
        <label class="login-field">
          <div class="login-field-head">
            <span>密码</span>
            <a class="login-link" @click.prevent="router.push('/forgot-password')">忘记密码？</a>
          </div>
          <Input v-model="password" type="password" placeholder="请输入密码" :disabled="loading" />
        </label>
        <button type="submit" class="login-btn" :disabled="loading">
          <span v-if="loading" class="login-btn-spinner"></span>
          <span v-else>登录</span>
        </button>
        <div class="login-links">
          <a class="login-link" @click.prevent="mode = 'token'">或使用 Token 登录</a>
          <router-link class="login-link" to="/register">注册账号</router-link>
        </div>
      </form>

      <form v-else class="login-form" @submit.prevent="onTokenLogin">
        <label class="login-field">
          <span>订阅 Token</span>
          <Input v-model="token" mono placeholder="sub_xxxxxxxx" :disabled="loading" />
        </label>
        <button type="submit" class="login-btn" :disabled="loading">
          <span v-if="loading" class="login-btn-spinner"></span>
          <span v-else>登录</span>
        </button>
        <div class="login-links">
          <a class="login-link" @click.prevent="mode = 'email'">返回邮箱登录</a>
        </div>
      </form>
    </div>
  </div>
</template>

<style scoped src="@/assets/styles/pages/user/Login.css"></style>
