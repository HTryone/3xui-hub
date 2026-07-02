<script setup>
/**
 * 订阅格式配置：管理订阅链接支持的格式（Clash、Sing-box 等）。
 */
import { ref, onMounted } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Input from '@/components/ui/Input.vue'
import Icon from '@/components/ui/Icon.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()
const loading = ref(false)
const saving = ref(false)

const form = ref({
  sub_clash_enabled: '0',
  sub_singbox_enabled: '0',
  sub_show_traffic: '0',
  sub_show_expire: '0',
  sub_show_flag: '0',
  sub_rename_enabled: '0',
  sub_rename_regex: '',
  sub_rename_replacement: '',
})

const load = async () => {
  loading.value = true
  try {
    const data = await adminApi.getSubscriptionSettings()
    form.value = { ...form.value, ...data }
  } catch (e) {
    toast.error('加载失败')
  } finally {
    loading.value = false
  }
}

const save = async () => {
  saving.value = true
  try {
    await adminApi.updateSubscriptionSettings(form.value)
    toast.success('保存成功')
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

const toggleFormat = (key) => {
  form.value[key] = form.value[key] === '1' ? '0' : '1'
}

onMounted(load)
</script>

<template>
  <div class="sub-settings">
    <h1 class="sub-settings-title">订阅配置</h1>

    <Card>
      <div class="sub-settings-form">
        <!-- 订阅格式开关 -->
        <div class="sub-settings-section">
          <h3 class="sub-settings-section-title">订阅格式</h3>
          <p class="sub-settings-section-desc">开启后用户的订阅链接将支持对应的格式</p>

          <div class="sub-settings-formats">
            <div
              class="sub-settings-format-card"
              :class="{ 'sub-settings-format-card--active': form.sub_clash_enabled === '1' }"
              @click="toggleFormat('sub_clash_enabled')"
            >
              <div class="sub-settings-format-icon">
                <img src="/images/clash.png" alt="Clash" class="sub-settings-format-img" />
              </div>
              <div class="sub-settings-format-info">
                <span class="sub-settings-format-name">Clash / FlClash</span>
                <span class="sub-settings-format-desc">Clash YAML 格式</span>
              </div>
              <div class="sub-settings-format-toggle">
                <div class="sub-settings-toggle" :class="{ 'sub-settings-toggle--on': form.sub_clash_enabled === '1' }">
                  <div class="sub-settings-toggle-knob"></div>
                </div>
              </div>
            </div>

            <div
              class="sub-settings-format-card"
              :class="{ 'sub-settings-format-card--active': form.sub_singbox_enabled === '1' }"
              @click="toggleFormat('sub_singbox_enabled')"
            >
              <div class="sub-settings-format-icon">
                <img src="/images/singbox.svg" alt="Sing-box" class="sub-settings-format-img" />
              </div>
              <div class="sub-settings-format-info">
                <span class="sub-settings-format-name">Sing-box / NekoBox</span>
                <span class="sub-settings-format-desc">Sing-box JSON 格式</span>
              </div>
              <div class="sub-settings-format-toggle">
                <div class="sub-settings-toggle" :class="{ 'sub-settings-toggle--on': form.sub_singbox_enabled === '1' }">
                  <div class="sub-settings-toggle-knob"></div>
                </div>
              </div>
            </div>
          </div>
        </div>

        <!-- 订阅内容控制 -->
        <div class="sub-settings-section">
          <h3 class="sub-settings-section-title">订阅内容</h3>
          <p class="sub-settings-section-desc">控制订阅链接中包含的信息</p>

          <div class="sub-settings-options">
            <label class="sub-settings-option">
              <input
                type="checkbox"
                :checked="form.sub_show_traffic === '1'"
                @change="form.sub_show_traffic = form.sub_show_traffic === '1' ? '0' : '1'"
              />
              <span>包含流量信息</span>
            </label>

            <label class="sub-settings-option">
              <input
                type="checkbox"
                :checked="form.sub_show_expire === '1'"
                @change="form.sub_show_expire = form.sub_show_expire === '1' ? '0' : '1'"
              />
              <span>包含到期时间</span>
            </label>
          </div>
        </div>

        <!-- 节点名称格式 -->
        <div class="sub-settings-section">
          <h3 class="sub-settings-section-title">节点名称</h3>
          <p class="sub-settings-section-desc">开启后节点名称自动显示为旗帜+地区+编号格式</p>

          <div class="sub-settings-options">
            <label class="sub-settings-option">
              <input
                type="checkbox"
                :checked="form.sub_show_flag === '1'"
                @change="form.sub_show_flag = form.sub_show_flag === '1' ? '0' : '1'"
              />
              <span>显示旗帜（如：🇭🇰 HK01）</span>
            </label>
          </div>
        </div>

        <!-- 正则重命名 -->
        <div class="sub-settings-section">
          <h3 class="sub-settings-section-title">正则重命名</h3>
          <p class="sub-settings-section-desc">使用正则表达式修改节点名称（高级功能）</p>

          <div class="sub-settings-options">
            <label class="sub-settings-option">
              <input
                type="checkbox"
                :checked="form.sub_rename_enabled === '1'"
                @change="form.sub_rename_enabled = form.sub_rename_enabled === '1' ? '0' : '1'"
              />
              <span>启用正则重命名</span>
            </label>
          </div>

          <div v-if="form.sub_rename_enabled === '1'" class="sub-settings-rename">
            <div class="sub-settings-field">
              <label class="sub-settings-field-label">正则表达式</label>
              <Input
                v-model="form.sub_rename_regex"
                placeholder="/HK-Mobile-(.*)/"
              />
              <p class="sub-settings-field-hint">匹配节点名称的正则表达式，如 /HK-(.*)/ 匹配 HK 开头的名称</p>
            </div>

            <div class="sub-settings-field">
              <label class="sub-settings-field-label">替换为</label>
              <Input
                v-model="form.sub_rename_replacement"
                placeholder="🇭🇰 $1"
              />
              <p class="sub-settings-field-hint">替换后的内容，$1 代表第一个捕获组，$2 代表第二个，以此类推</p>
            </div>
          </div>
        </div>

        <div class="sub-settings-actions">
          <Button variant="primary" size="sm" :disabled="saving" @click="save">
            <Icon name="check" :size="16" /> {{ saving ? '保存中...' : '保存' }}
          </Button>
        </div>
      </div>
    </Card>
  </div>
</template>

<style scoped>
.sub-settings-title {
  font-size: var(--text-xl);
  font-weight: var(--font-semibold);
  margin-bottom: var(--space-4);
}
.sub-settings-form {
  display: flex;
  flex-direction: column;
  gap: var(--space-6);
}
.sub-settings-section {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.sub-settings-section-title {
  font-size: var(--text-base);
  font-weight: var(--font-semibold);
  margin: 0;
}
.sub-settings-section-desc {
  font-size: var(--text-sm);
  color: var(--text-muted);
  margin: 0;
}
.sub-settings-formats {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.sub-settings-format-card {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-3) var(--space-4);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-lg);
  cursor: pointer;
  transition: all 0.2s;
}
.sub-settings-format-card:hover {
  border-color: var(--accent);
}
.sub-settings-format-card--active {
  border-color: var(--accent);
  background: var(--accent-muted);
}
.sub-settings-format-icon {
  width: 40px;
  height: 40px;
  display: flex;
  align-items: center;
  justify-content: center;
  background: var(--bg-hover);
  border-radius: var(--radius-md);
  color: var(--text-secondary);
}
.sub-settings-format-img {
  width: 28px;
  height: 28px;
  object-fit: contain;
}
.sub-settings-format-card--active .sub-settings-format-icon {
  background: var(--accent);
  color: #fff;
}
.sub-settings-format-info {
  flex: 1;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.sub-settings-format-name {
  font-size: var(--text-sm);
  font-weight: var(--font-medium);
}
.sub-settings-format-desc {
  font-size: var(--text-xs);
  color: var(--text-muted);
}
.sub-settings-toggle {
  width: 44px;
  height: 24px;
  background: var(--bg-hover);
  border-radius: 12px;
  position: relative;
  transition: background 0.2s;
}
.sub-settings-toggle--on {
  background: var(--accent);
}
.sub-settings-toggle-knob {
  width: 20px;
  height: 20px;
  background: #fff;
  border-radius: 50%;
  position: absolute;
  top: 2px;
  left: 2px;
  transition: transform 0.2s;
  box-shadow: 0 1px 3px rgba(0,0,0,0.1);
}
.sub-settings-toggle--on .sub-settings-toggle-knob {
  transform: translateX(20px);
}
.sub-settings-options {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.sub-settings-option {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  cursor: pointer;
  font-size: var(--text-sm);
}
.sub-settings-option input[type="checkbox"] {
  width: 16px;
  height: 16px;
  accent-color: var(--accent);
}
.sub-settings-field {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}
.sub-settings-field-hint {
  font-size: var(--text-xs);
  color: var(--text-muted);
  margin: 0;
}
.sub-settings-rename {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  margin-top: var(--space-3);
  padding-top: var(--space-3);
  border-top: 1px solid var(--border-subtle);
}
.sub-settings-field-label {
  font-size: var(--text-sm);
  font-weight: var(--font-medium);
  color: var(--text-secondary);
}
.sub-settings-actions {
  padding-top: var(--space-2);
}
</style>
