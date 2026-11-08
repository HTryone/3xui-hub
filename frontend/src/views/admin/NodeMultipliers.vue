<script setup>
/**
 * Admin 节点流量倍率：原始倍率（继承的节点用）+ 单节点覆盖。
 *
 * 口径：生效倍率 = 节点倍率 ?? 原始倍率。
 * 改原始倍率只影响「继承中」的节点 —— 手动指定过的节点永远不受影响，
 * 所以这里没有任何「批量应用到全部节点」的操作，那与原始倍率的概念冲突。
 */
import { ref, computed, onMounted, onActivated } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Input from '@/components/ui/Input.vue'
import Modal from '@/components/ui/Modal.vue'
import Icon from '@/components/ui/Icon.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()

const nodes = ref([])
const originalInput = ref('1.0')
const savingOriginal = ref(false)
const savedOriginal = ref(false)
const showMultiplier = ref(false)
const savingToggle = ref(false)

const modalOpen = ref(false)
const savedNode = ref(false)
const editing = ref(null)
// 编辑弹窗里的倍率输入；空串 = 继承原始倍率
const editInput = ref('')

const originalMultiplier = computed(() => {
  const parsed = parseFloat(originalInput.value)
  return Number.isFinite(parsed) ? parsed : 1.0
})

const inheritedCount = computed(() => nodes.value.filter(n => n.traffic_multiplier == null).length)

const load = async () => {
  try {
    // 倍率要逐个设到每个节点，必须一次看到全部 —— 显式 all=1 拿全量（列表页才走分页）
    nodes.value = await adminApi.listNodes({ all: 1 })
    const anyNode = nodes.value[0]
    // original_multiplier / show_multiplier 每行都带，取任意一条即可
    // （没有任何节点时保持输入框与开关的现值不动）
    if (anyNode) {
      if (anyNode.original_multiplier != null) {
        originalInput.value = String(anyNode.original_multiplier)
      }
      showMultiplier.value = !!anyNode.show_multiplier
    }
  } catch (e) {
    toast.error('加载失败')
  }
}

const saveOriginal = async () => {
  const value = parseFloat(originalInput.value)
  if (!Number.isFinite(value) || value < 1 || value > 100) {
    toast.error('倍率需在 1 ~ 100 之间')
    return
  }
  savingOriginal.value = true
  try {
    await adminApi.setOriginalNodeMultiplier(value)
    toast.success('原始倍率已保存，仅影响继承中的节点')
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段）
    savedOriginal.value = true
    setTimeout(() => { savedOriginal.value = false }, 900)
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    savingOriginal.value = false
  }
}

const toggleDisplay = async () => {
  const next = !showMultiplier.value
  savingToggle.value = true
  try {
    await adminApi.setNodeMultiplierDisplay(next)
    showMultiplier.value = next
    toast.success(next ? '用户端节点列表将展示倍率' : '用户端不再展示倍率')
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    savingToggle.value = false
  }
}

const openEdit = (n) => {
  editing.value = n
  editInput.value = n.traffic_multiplier == null ? '' : String(n.traffic_multiplier)
  modalOpen.value = true
}

const saveNode = async () => {
  const raw = editInput.value.trim()
  let payload
  if (raw === '') {
    // 清空 = 改回继承原始倍率
    payload = null
  } else {
    const value = parseFloat(raw)
    if (!Number.isFinite(value) || value < 1 || value > 100) {
      toast.error('倍率需在 1 ~ 100 之间，或留空改回继承')
      return
    }
    payload = value
  }

  try {
    await adminApi.updateNode(editing.value.id, { traffic_multiplier: payload })
    toast.success(payload === null ? '已改回继承原始倍率' : '已保存')
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段），再关弹窗
    savedNode.value = true
    setTimeout(() => {
      savedNode.value = false
      modalOpen.value = false
    }, 900)
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  }
}

const clearToInherit = async (n) => {
  if (!confirm(`将 ${n.name} 改回继承原始倍率？`)) return
  try {
    await adminApi.updateNode(n.id, { traffic_multiplier: null })
    toast.success('已改回继承原始倍率')
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  }
}

const statusVariant = (s) => ({ online: 'success', offline: 'danger', maintenance: 'warning' }[s] || 'info')

// 倍率列：继承的显示实际生效值 + 「继承」标记，手动设的显示数值 + 「单独设置」标记
const effective = (n) => (n.traffic_multiplier == null ? (n.original_multiplier ?? 1.0) : n.traffic_multiplier)
const isInherited = (n) => n.traffic_multiplier == null

onMounted(load)
onActivated(load)
</script>

<template>
  <div class="an">
    <div class="an-head">
      <h1 class="ch-page-title">流量倍率</h1>
    </div>

    <p class="an-hint">
      倍率只作用于<strong>本次新增</strong>的流量增量，不追溯已统计的历史流量。
      快照始终保存面板原始字节数，所以改倍率不会导致用户流量突然暴涨。
    </p>

    <Card>
      <div class="an-original">
        <label class="ch-field an-original-field">
          <span>原始倍率</span>
          <Input v-model="originalInput" type="number" step="0.01" min="1" max="100" mono placeholder="1.0" />
        </label>
        <div class="an-original-side">
          <Button variant="primary" size="sm" :loading="savingOriginal" :success="savedOriginal" @click="saveOriginal">
            保存
          </Button>
        </div>
        <label class="ch-field an-toggle">
          <span class="an-toggle-row">
            <input type="checkbox" :checked="showMultiplier" :disabled="savingToggle" @change="toggleDisplay" />
            <span>用户端展示节点倍率</span>
          </span>
          <span class="an-muted">关闭时用户端接口不返回该字段</span>
        </label>
      </div>
    </Card>

    <Card :padding="false" class="an-table-card">
      <div class="an-table-wrap">
        <table class="an-table">
          <thead>
            <tr>
              <th>节点</th>
              <th>状态</th>
              <th>倍率</th>
              <th>来源</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="n in nodes" :key="n.id">
              <td>{{ n.name }}</td>
              <td><Badge :variant="statusVariant(n.status)" dot>{{ n.status }}</Badge></td>
              <td class="mono">{{ effective(n) }}</td>
              <td>
                <Badge :variant="isInherited(n) ? 'info' : 'warning'">
                  {{ isInherited(n) ? '继承' : '单独设置' }}
                </Badge>
              </td>
              <td>
                <div class="an-actions">
                  <Button variant="ghost" size="sm" @click="openEdit(n)"><Icon name="edit" :size="16" /></Button>
                  <Button
                    v-if="!isInherited(n)"
                    variant="ghost"
                    size="sm"
                    title="改回继承原始倍率"
                    @click="clearToInherit(n)"
                  >
                    <Icon name="refresh" :size="16" />
                  </Button>
                </div>
              </td>
            </tr>
            <tr v-if="!nodes.length">
              <td colspan="5" class="an-empty">暂无节点</td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <Modal v-model="modalOpen" :title="`设置倍率 · ${editing?.name || ''}`" width="440px">
      <div class="ch-form">
        <p class="an-muted">
          当前来源：<strong>{{ editing && isInherited(editing) ? '继承' : '单独设置' }}</strong>
          <template v-if="editing && isInherited(editing)">
            ，生效倍率 {{ effective(editing) }}
          </template>
        </p>
        <label class="ch-field">
          <span>节点倍率（留空 = 改回继承原始倍率）</span>
          <Input v-model="editInput" type="number" step="0.01" min="1" max="100" mono placeholder="留空即继承" />
        </label>
        <p class="an-muted">
          手动设置后，该节点不再跟随原始倍率变化；倍率只影响以后新增的流量。
        </p>
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" @click="modalOpen = false">取消</Button>
        <Button variant="primary" size="sm" :success="savedNode" @click="saveNode">保存</Button>
      </template>
    </Modal>
  </div>
</template>

<style scoped src="@/assets/styles/pages/admin/NodeMultipliers.css"></style>
