<script setup>
/**
 * Admin · App 版本分发（精简版）。
 *
 * 只保留最基础的 4 项：版本号 / 更新说明 / 下载地址 / 强制更新开关。
 * App 启动时 GET /api/app-version 读同一批键做升级决策。
 */
import { ref, onMounted } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Input from '@/components/ui/Input.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()

const form = ref({
  app_latest_version_name: '',
  app_update_notes: '',
  app_download_url: '',
  app_force_update: '0',
})

const loading = ref(false)
const saving = ref(false)
const saved = ref(false)

const load = async () => {
  loading.value = true
  try {
    const data = await adminApi.getAppVersionSettings()
    form.value = { ...form.value, ...data }
  } catch (e) {
    toast.error(e.message || '加载失败')
  } finally {
    loading.value = false
  }
}

const save = async () => {
  const f = form.value
  saving.value = true
  try {
    await adminApi.updateAppVersionSettings({
      app_latest_version_name: f.app_latest_version_name,
      app_update_notes: f.app_update_notes,
      app_download_url: f.app_download_url,
      app_force_update: f.app_force_update === '1' || f.app_force_update === true,
    })
    toast.success('已保存')
    saved.value = true
    setTimeout(() => { saved.value = false }, 900)
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

const savingForce = ref(false)

// 强制更新开关：改了就自动保存，不用点「保存设置」。
const toggleForce = async (checked) => {
  form.value.app_force_update = checked ? '1' : '0'
  savingForce.value = true
  try {
    await adminApi.updateAppVersionSettings({ app_force_update: checked })
    toast.success(checked ? '已开启强制更新' : '已关闭强制更新')
  } catch (e) {
    // 保存失败回滚开关状态。
    form.value.app_force_update = checked ? '0' : '1'
    toast.error(e.message || '保存失败')
  } finally {
    savingForce.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="av">
    <div class="av-head">
      <div>
        <h1 class="ch-page-title--mb">App 版本</h1>
        <p class="av-sub">配置 App 升级规则，留空则不提示更新。</p>
      </div>
      <Button variant="primary" size="sm" :loading="saving" :success="saved" @click="save">保存设置</Button>
    </div>

    <Card class="av-card">
      <h2 class="av-section">升级配置</h2>
      <p class="av-tip av-tip-block">版本号留空 = 站点未配置分发，App 端静默不弹更新。</p>
      <div class="av-grid">
        <label class="ch-field-6">
          <span class="av-label">版本号</span>
          <Input v-model="form.app_latest_version_name" placeholder="1.2.0" />
        </label>
        <label class="ch-field-6">
          <span class="av-label">下载地址</span>
          <Input v-model="form.app_download_url" placeholder="https://example.com/app.apk" />
        </label>
        <label class="ch-field-6 av-field-full">
          <span class="av-label">更新说明</span>
          <textarea v-model="form.app_update_notes" class="av-textarea" rows="4" placeholder="修复连接超时&#10;优化测速"></textarea>
        </label>
        <div class="ch-field-6">
          <span class="av-label">强制更新</span>
          <label class="av-switch">
            <input
              type="checkbox"
              class="av-switch-input"
              :checked="form.app_force_update === '1' || form.app_force_update === true"
              :disabled="savingForce"
              @change="toggleForce($event.target.checked)"
            />
            <span class="av-switch-slider"></span>
            <span class="av-switch-text">{{ savingForce ? '保存中…' : '开启后所有旧版本都强制升级' }}</span>
          </label>
        </div>
      </div>
    </Card>
  </div>
</template>

<style scoped src="@/assets/styles/pages/admin/AppVersion.css"></style>
