<script setup>
/**
 * Admin 支付配置：表格 + 创建/编辑 Modal + 删除。
 * 协议字段由后台 /admin-api/payments/protocols 下发，按所选协议动态渲染；
 * 通用字段（名称/回调地址/启用）保持现有交互。
 */
import { ref, computed, onMounted, onActivated } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import Modal from '@/components/ui/Modal.vue'
import Icon from '@/components/ui/Icon.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()
const payments = ref([])
const protocols = ref([])
const protocolOptions = computed(() => protocols.value.map((p) => ({ label: p.label, value: p.key })))
const loading = ref(false)

/* ===== 全局设置（只有两项）=====
 * 1) 订单支付有效期：一个值管所有渠道。订单过期时刻 = 下单时间 + 它；配得比网关长会发死链，故上限 120。
 *    缺省 5 分钟，与后端 payment.DefaultTTLMinutes 同值。
 * 2) 允许继续支付：关掉后用户端订单不显示「继续支付」，且后端不缓存、不响应续付（整套关闭）。
 * （兜底作废任务每小时自动跑，只做低风险扫地，不需要开关。） */
const ttlMinutes = ref(5)
const ttlSaving = ref(false)
const ttlInput = ref('5')

const continuePayEnabled = ref(true)
const continuePayToggling = ref(false)

const loadSettings = async () => {
  try {
    const res = await adminApi.getPaymentSettings()
    ttlMinutes.value = Number(res?.pay_ttl_minutes ?? 5)
    ttlInput.value = String(ttlMinutes.value)
    continuePayEnabled.value = res?.continue_pay_enabled !== false
  } catch (e) {
    toast.error('支付设置加载失败')
  }
}

const saveSettings = async () => {
  const n = Number(String(ttlInput.value).trim())
  if (!Number.isInteger(n) || n < 1 || n > 120) {
    toast.error('支付有效期需为 1~120 之间的整数（分钟）')
    return
  }
  if (n === ttlMinutes.value) {
    toast.info('无变更')
    return
  }
  ttlSaving.value = true
  try {
    const res = await adminApi.updatePaymentSettings({ pay_ttl_minutes: n })
    ttlMinutes.value = Number(res?.pay_ttl_minutes ?? n)
    ttlInput.value = String(ttlMinutes.value)
    toast.success(`已更新：支付有效期 ${ttlMinutes.value} 分钟`)
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    ttlSaving.value = false
  }
}

// 开关即时生效（与「自动备份」开关同款交互），不回退成需要点保存
const toggleContinuePay = async () => {
  const next = !continuePayEnabled.value
  continuePayToggling.value = true
  try {
    const res = await adminApi.updatePaymentSettings({ continue_pay_enabled: next })
    continuePayEnabled.value = res?.continue_pay_enabled !== false
    toast.success(continuePayEnabled.value ? '已允许用户继续支付' : '已关闭：用户端将不显示「继续支付」')
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    continuePayToggling.value = false
  }
}

const modalOpen = ref(false)
const editing = ref(null)
const form = ref({
  name: '',
  driver_type: 'payindex',
  fields: {},
  notify_url: '',
  enabled: true,
})

// 旧列字段名（与 payment_configs 表列名一致）：保存时镜像一份，老读取路径零影响
const LEGACY_KEYS = ['gateway', 'query_gateway', 'member_id', 'api_key', 'bank_code']

// 通用字段（不在协议 fields 里）的中文名：变更提示用
const COMMON_LABELS = { name: '配置名称', driver_type: '支付协议', notify_url: '回调地址', enabled: '启用状态' }

// 打开编辑时回填的原始值快照：保存成功后按它算出「这次实际改了哪些字段」
const original = ref(null)

// 当前协议定义（老数据 driver_type 为空按 payindex 处理）
const currentProtocol = computed(() => {
  const key = form.value.driver_type || 'payindex'
  return protocols.value.find((p) => p.key === key) || protocols.value[0] || null
})

const protocolLabel = (t) => {
  const key = t || 'payindex'
  return protocols.value.find((p) => p.key === key)?.label || key
}

// 字段名 → 中文名：先按当前协议定义，再按任一协议定义，最后通用字段/字段名兜底
const labelOf = (key) => {
  const hit =
    (currentProtocol.value?.fields || []).find((f) => f.name === key) ||
    protocols.value.flatMap((p) => p.fields || []).find((f) => f.name === key)
  return hit?.label || COMMON_LABELS[key] || key
}

// 与后端口径一致（config 值落库前 trim），避免只有首尾空格的“假变更”
const norm = (v) => String(v ?? '').trim()

/**
 * 本次实际变更的字段中文名（顺序：名称/协议/协议字段/回调地址/启用）。
 * 密文留空 = 不修改：提交前已从 fields 里剔除，与原始空值比相等 → 不算变更。
 */
const diffLabels = (before, now) => {
  const labels = []
  const add = (key) => {
    const label = labelOf(key)
    if (!labels.includes(label)) labels.push(label)
  }
  const beforeFields = before?.fields || {}

  if (norm(before?.name) !== norm(now.name)) add('name')
  if (norm(before?.driver_type) !== norm(now.driver_type)) add('driver_type')

  // 协议字段按当前协议定义顺序，再补两边多出来的键（切协议时旧键仍在表单里）
  const keys = (currentProtocol.value?.fields || []).map((f) => f.name)
  for (const k of Object.keys({ ...beforeFields, ...now.fields })) {
    if (!keys.includes(k)) keys.push(k)
  }
  for (const k of keys) {
    if (norm(beforeFields[k]) !== norm(now.fields[k])) add(k)
  }

  if (norm(before?.notify_url) !== norm(now.notify_url)) add('notify_url')
  if (!!before?.enabled !== !!now.enabled) add('enabled')
  return labels
}

const load = async () => {
  loading.value = true
  try {
    payments.value = await adminApi.listPayments()
  } catch (e) {
    toast.error('加载失败')
  } finally {
    loading.value = false
  }
}

const loadProtocols = async () => {
  try {
    const list = await adminApi.listPaymentProtocols()
    protocols.value = Array.isArray(list) ? list : []
  } catch (e) {
    protocols.value = []
    toast.error('协议清单加载失败')
  }
}

const resetForm = () => {
  form.value = {
    name: '',
    driver_type: protocols.value[0]?.key || 'payindex',
    fields: {},
    notify_url: '',
    enabled: true,
  }
  original.value = null
}

const openCreate = () => {
  editing.value = null
  resetForm()
  modalOpen.value = true
}

const openEdit = async (p) => {
  editing.value = p
  resetForm()
  try {
    const detail = await adminApi.getPayment(p.id)
    // 老数据的字段在旧列里，新数据在 driver_config 里：driver_config 优先，合并回显
    const fields = {}
    for (const k of LEGACY_KEYS) fields[k] = detail[k] || ''
    Object.assign(fields, detail.driver_config || {})
    fields.api_key = '' // 密钥不回显，留空表示不修改
    form.value = {
      name: detail.name,
      driver_type: detail.driver_type || 'payindex',
      fields,
      notify_url: detail.notify_url || '',
      enabled: detail.enabled,
    }
    // 回填后的这份就是“编辑前原始值”，保存后拿它算变更清单
    original.value = {
      name: form.value.name,
      driver_type: form.value.driver_type,
      notify_url: form.value.notify_url,
      enabled: !!form.value.enabled,
      fields: { ...fields },
    }
  } catch (e) {
    toast.error('加载配置详情失败')
  }
  modalOpen.value = true
}

const save = async () => {
  if (!form.value.name) {
    toast.error('请填写配置名称')
    return
  }

  // 必填校验按当前协议定义来（编辑时密文字段留空 = 不修改，跳过校验）
  const isEdit = !!editing.value
  for (const f of currentProtocol.value?.fields || []) {
    const v = String(form.value.fields[f.name] ?? '').trim()
    if (f.required && !v && !(isEdit && f.isSecret)) {
      toast.error(`请填写${f.label}`)
      return
    }
  }

  // 编辑时密文留空 = 不修改：整批按 isSecret 通用处理（api_key/应用私钥/支付宝公钥等）
  const fields = { ...form.value.fields }
  if (isEdit) {
    for (const f of currentProtocol.value?.fields || []) {
      if (f.isSecret && !String(fields[f.name] ?? '').trim()) {
        delete fields[f.name]
      }
    }
  }

  const payload = {
    name: form.value.name,
    enabled: form.value.enabled,
    notify_url: form.value.notify_url,
    driver_type: form.value.driver_type,
    driver_config: fields,
  }
  // 同名旧列镜像一份（两种协议的字段名都落在旧列上，列表/老代码读旧列不受影响）
  for (const k of LEGACY_KEYS) {
    if (fields[k] !== undefined && fields[k] !== '') payload[k] = fields[k]
    else if (k !== 'api_key') payload[k] = fields[k] ?? ''
  }

  // 变更清单：按提交给后端的 payload 与打开编辑时的原始值逐字段比；新建不算
  const changed =
    isEdit && original.value
      ? diffLabels(original.value, {
          name: payload.name,
          driver_type: payload.driver_type,
          notify_url: payload.notify_url,
          enabled: payload.enabled,
          fields: payload.driver_config,
        })
      : []

  try {
    if (isEdit) {
      await adminApi.updatePayment(editing.value.id, payload)
      if (changed.length) toast.success(`已更新：${changed.join('、')}`)
      else toast.info('无变更')
    } else {
      await adminApi.createPayment(payload)
      toast.success('已创建')
    }
    modalOpen.value = false
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  }
}

const remove = async (p) => {
  if (!confirm(`删除支付配置「${p.name}」？`)) return
  try {
    await adminApi.deletePayment(p.id)
    toast.success('已删除')
    await load()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

onMounted(() => {
  load()
  loadProtocols()
  loadSettings()
})
onActivated(load)
</script>

<template>
  <div class="pay">
    <div class="pay-head">
      <h1 class="ch-page-title">支付配置</h1>
      <Button variant="primary" size="sm" @click="openCreate"><Icon name="plus" :size="16" /> 新建配置</Button>
    </div>

    <!-- 全局设置：一个值管所有渠道（不分支付方式）；开关在有效期输入框的右侧 -->
    <Card>
      <div class="pay-settings">
        <label class="pay-settings-field">
          <span>订单支付有效期（分钟）</span>
          <Input v-model="ttlInput" mono placeholder="5" class="pay-settings-input" />
        </label>
        <Button variant="primary" size="sm" :loading="ttlSaving" @click="saveSettings">保存</Button>

        <div class="pay-settings-task">
          <span class="pay-settings-task-label">允许继续支付</span>
          <label class="ch-switch" :title="continuePayEnabled ? '点击关闭' : '点击开启'">
            <input
              type="checkbox"
              class="ch-switch-input"
              :checked="continuePayEnabled"
              :disabled="continuePayToggling"
              @change="toggleContinuePay"
            />
            <span class="ch-switch-slider"></span>
          </label>
        </div>

        <p class="pay-settings-hint">
          当前 {{ ttlMinutes }} 分钟。下单时按它算出订单的支付截止时间，用户端据此倒计时；
          <b>不要超过网关支付链接本身的有效期</b>（常见 5 或 10 分钟），否则用户会拿到已失效的链接。
          <b>「允许继续支付」</b>关掉后，用户端「我的订单」不再显示「继续支付」，后端也不再缓存/响应续付
          （有的网关链接不支持复用，可据此整套关闭）。
        </p>
      </div>
    </Card>

    <Card padding="false">
      <div class="pay-table-wrap">
        <table class="ch-table">
          <thead>
            <tr>
              <th>名称</th>
              <th>协议</th>
              <th>商户号</th>
              <th>银行编码</th>
              <th>网关地址</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="p in payments" :key="p.id">
              <td>{{ p.name }}</td>
              <td>{{ protocolLabel(p.driver_type) }}</td>
              <td class="mono">{{ p.member_id }}</td>
              <td class="mono">{{ p.bank_code }}</td>
              <td class="pay-url">{{ p.gateway }}</td>
              <td><Badge :variant="p.enabled ? 'success' : 'danger'" dot>{{ p.enabled ? '启用' : '禁用' }}</Badge></td>
              <td>
                <div class="ch-actions">
                  <Button variant="ghost" size="sm" @click="openEdit(p)"><Icon name="edit" :size="14" /></Button>
                  <Button variant="ghost" size="sm" @click="remove(p)"><Icon name="trash" :size="14" /></Button>
                </div>
              </td>
            </tr>
            <tr v-if="!payments.length">
              <td colspan="7" class="ch-empty">暂无支付配置</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Modal v-model="modalOpen" :title="editing ? '编辑支付配置' : '新建支付配置'" width="560px">
      <div class="ch-form">
        <label class="pay-field">
          <span>配置名称</span>
          <Input v-model="form.name" placeholder="如：微信支付" />
        </label>

        <label class="pay-field">
          <span>支付协议</span>
          <Select v-model="form.driver_type" :options="protocolOptions" />
        </label>
        <p v-if="currentProtocol?.description" class="pay-desc">{{ currentProtocol.description }}</p>

        <!-- 协议字段：label / placeholder / required / 密文框全部来自后台协议定义 -->
        <label v-for="f in currentProtocol?.fields || []" :key="f.name" class="pay-field">
          <span>
            {{ f.label }}<template v-if="editing && f.isSecret">（留空不修改）</template><template v-else-if="!f.required">（选填）</template>
          </span>
          <Input
            v-model="form.fields[f.name]"
            :type="f.isSecret ? 'password' : 'text'"
            mono
            :placeholder="editing && f.isSecret ? '留空则不修改' : f.placeholder"
          />
        </label>

        <label class="pay-field">
          <span>回调地址（留空使用系统默认）</span>
          <Input v-model="form.notify_url" mono placeholder="留空则自动生成" />
        </label>
        <label class="pay-field ch-field-row">
          <input v-model="form.enabled" type="checkbox" />
          <span>启用</span>
        </label>
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" @click="modalOpen = false">取消</Button>
        <Button variant="primary" size="sm" @click="save">保存</Button>
      </template>
    </Modal>
  </div>
</template>

<style scoped src="@/assets/styles/pages/admin/Payments.css"></style>
