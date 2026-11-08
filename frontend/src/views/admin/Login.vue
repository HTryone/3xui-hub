<script setup>
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import Card from '@/components/ui/Card.vue'
import Input from '@/components/ui/Input.vue'
import Button from '@/components/ui/Button.vue'
import Icon from '@/components/ui/Icon.vue'
import { useAdminStore } from '@/stores/admin'
import { useSiteConfigStore } from '@/stores/siteConfig'
import { useToast } from '@/components/ui/Toast.vue'

const admin = useAdminStore()
const siteConfig = useSiteConfigStore()
const router = useRouter()
const toast = useToast()

const username = ref('')
const password = ref('')
const otp = ref('')
const needOtp = ref(false)
const loading = ref(false)

const onSubmit = async () => {
  loading.value = true
  try {
    await admin.login(username.value.trim(), password.value, otp.value.trim())
    toast.success('登录成功')
    router.push('/admin')
  } catch (e) {
    if (e.message && e.message.includes('验证码')) {
      needOtp.value = true
    }
    toast.error(e.message || '登录失败')
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="al-login">
    <div class="login-bg">
      <div class="login-bg-circle login-bg-circle--1"></div>
      <div class="login-bg-circle login-bg-circle--2"></div>
    </div>

    <Card>
      <div class="al-login-head">
        <span class="al-login-logo">
          <img v-if="siteConfig.logo" :src="siteConfig.logo" alt="logo" class="al-login-logo-img" />
          <Icon v-else name="shield" :size="24" />
        </span>
        <h1 class="al-login-title">{{ siteConfig.title }} Admin</h1>
      </div>
      <form class="al-login-form" @submit.prevent="onSubmit">
        <label class="al-label">用户名</label>
        <Input v-model="username" placeholder="admin" :disabled="loading" />
        <label class="al-label">密码</label>
        <Input v-model="password" type="password" placeholder="••••••" :disabled="loading" />
        <template v-if="needOtp">
          <label class="al-label">谷歌验证码</label>
          <Input v-model="otp" mono placeholder="6位动态验证码" maxlength="6" :disabled="loading" />
        </template>
        <Button type="submit" variant="primary" :loading="loading" class="al-login-btn">登录</Button>
      </form>
    </Card>
  </div>
</template>

<style scoped src="@/assets/styles/pages/admin/Login.css"></style>
