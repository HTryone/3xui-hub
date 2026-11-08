<script setup>
/**
 * 找回密码 — 与登录/注册同款精致优雅风格
 */
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import Input from '@/components/ui/Input.vue'
import Icon from '@/components/ui/Icon.vue'
import ThemeToggle from '@/components/ui/ThemeToggle.vue'
import { useSiteConfigStore } from '@/stores/siteConfig'
import { userApi } from '@/api/user'
import { useToast } from '@/components/ui/Toast.vue'

const siteConfig = useSiteConfigStore()
const router = useRouter()
const toast = useToast()

const email = ref('')
const emailCode = ref('')
const password = ref('')
const confirmPassword = ref('')
const loading = ref(false)

// 邮箱验证码相关
const emailCodeSending = ref(false)
const emailCodeCountdown = ref(0)
let emailCodeTimer = null

const onReset = async () => {
  if (!email.value.trim()) { toast.error('请输入邮箱'); return }
  if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email.value.trim())) { toast.error('邮箱格式不正确'); return }
  if (password.value.length < 6) { toast.error('密码至少 6 位'); return }
  if (password.value !== confirmPassword.value) { toast.error('两次密码不一致'); return }
  if (!emailCode.value.trim()) { toast.error('请输入邮箱验证码'); return }

  loading.value = true
  try {
    await userApi.resetPassword(email.value.trim(), emailCode.value.trim(), password.value)
    toast.success('密码已重置，请重新登录')
    router.push('/login')
  } catch (e) {
    toast.error(e.message || '重置失败')
  } finally { loading.value = false }
}

const sendEmailCode = async () => {
  if (!email.value.trim()) { toast.error('请输入邮箱'); return }
  if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email.value.trim())) { toast.error('邮箱格式不正确'); return }
  emailCodeSending.value = true
  try {
    await userApi.sendResetCode(email.value.trim())
    emailCodeCountdown.value = 60
    emailCodeTimer = setInterval(() => {
      emailCodeCountdown.value--
      if (emailCodeCountdown.value <= 0) {
        clearInterval(emailCodeTimer)
        emailCodeTimer = null
      }
    }, 1000)
    toast.success('验证码已发送')
  } catch (e) {
    toast.error(e.message || '发送失败')
  } finally { emailCodeSending.value = false }
}
</script>

<template>
  <div class="fp-page">
    <div class="fp-bg">
      <div class="fp-bg-circle fp-bg-circle--1"></div>
      <div class="fp-bg-circle fp-bg-circle--2"></div>
    </div>
    <div class="fp-top">
      <router-link to="/" class="fp-back"><Icon name="chevron-left" :size="16" /><span>返回首页</span></router-link>
      <ThemeToggle />
    </div>
    <div class="fp-card">
      <div class="fp-head">
        <span class="fp-logo">
          <img v-if="siteConfig.logo" :src="siteConfig.logo" alt="" class="fp-logo-img" />
          <Icon v-else name="shield" :size="24" />
        </span>
        <h1 class="fp-title">找回密码</h1>
        <p class="fp-sub">通过邮箱验证码重置密码</p>
      </div>
      <form class="fp-form" @submit.prevent="onReset">
        <label class="fp-field">
          <span>邮箱</span>
          <Input v-model="email" type="email" placeholder="name@example.com" :disabled="loading" />
        </label>
        <label class="fp-field">
          <div class="fp-field-head">
            <span>邮箱验证码</span>
            <span class="fp-field-tip">收不到邮箱？检查垃圾箱</span>
          </div>
          <div class="fp-captcha-row">
            <Input v-model="emailCode" placeholder="输入邮箱验证码" :disabled="loading" class="fp-captcha-input" />
            <button type="button" class="fp-send-code-btn" :disabled="emailCodeSending || emailCodeCountdown > 0" @click="sendEmailCode">
              {{ emailCodeCountdown > 0 ? `${emailCodeCountdown}s` : '发送验证码' }}
            </button>
          </div>
        </label>
        <label class="fp-field"><span>新密码</span><Input v-model="password" type="password" placeholder="至少 6 位" :disabled="loading" /></label>
        <label class="fp-field"><span>确认新密码</span><Input v-model="confirmPassword" type="password" placeholder="再次输入新密码" :disabled="loading" /></label>
        <button type="submit" class="fp-btn" :disabled="loading">
          <span v-if="loading" class="fp-btn-spinner"></span><span v-else>重置密码</span>
        </button>
        <div class="fp-links"><router-link class="fp-link" to="/login">想起密码了？去登录</router-link></div>
      </form>
    </div>
  </div>
</template>

<style scoped src="@/assets/styles/pages/user/ForgotPassword.css"></style>
