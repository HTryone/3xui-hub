<script setup>
/**
 * Tutorials — 管理员教程管理
 *
 * 排序（2026-11-06 用户定稿）：后台排的就是**用户端**的顺序（GET /api/tutorials 按 sort 升序）。
 *   - 组内拖拽：松手即把组内 sort 重写为 10/20/30…（批量 PUT，只改 sort 不碰标题内容）
 *   - 字母排序：组标题旁的按钮，按标题 localeCompare 写回 sort（用户端同步变）
 *   - 两种方式改的都是同一个 sort 字段，用户端顺序立即生效
 */
import { ref, computed, onMounted, onActivated } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'
import Icon from '@/components/ui/Icon.vue'
import { marked } from 'marked'
import DOMPurify from 'dompurify'

const route = useRoute()
const router = useRouter()
const toast = useToast()

const tutorials = ref([])
const editing = ref(null) // null = 列表，{} = 新建/编辑
const form = ref({ title: '', content: '', sort: 0, enabled: true })
const saving = ref(false)
const saved = ref(false)
/** 编辑页预览：按钮切换整页「编辑 ⇄ 预览」（预览时隐藏编辑器，只看渲染结果） */
const previewMode = ref(false)
/** 拖拽状态：draggingId = 正被拖的教程 id；overId = 当前悬停目标 id */
const draggingId = ref(null)
const overId = ref(null)
/** 批量保存中（拖拽落定 / 字母排序按钮共用） */
const reordering = ref(false)

const load = async () => {
  try { tutorials.value = await adminApi.listTutorials() } catch (e) { tutorials.value = [] }
}

/**
 * 按分类分组展示。
 * - 组内：sort 升序，再按 id 兜底，保证同 sort 值时顺序稳定不跳动
 * - 组间：**按组内最小 sort 升序** —— 「分类排序」保存的是
 *   sort = (组序)*100 + (组内位置)*10，组间顺序就编码在 sort 前缀里。
 *   （此前这里用分类名拼音排，会把用户刚设的分类顺序冲掉，重新打开面板又变回字母序。）
 * 无分类的归到「默认」组，不丢数据。
 */
const grouped = computed(() => {
  const map = new Map()
  for (const t of tutorials.value) {
    const cat = (t.category || '').trim() || '默认'
    if (!map.has(cat)) map.set(cat, [])
    map.get(cat).push(t)
  }
  return Array.from(map.entries())
    .map(([category, items]) => {
      const sorted = items.slice().sort((a, b) => (a.sort ?? 0) - (b.sort ?? 0) || a.id - b.id)
      return { category, items: sorted, minSort: sorted.length ? (sorted[0].sort ?? 0) : 0 }
    })
    .sort((a, b) => a.minSort - b.minSort)
})

/** 现有分类清单（分类输入框的 datalist 建议；去重 + 排除空）。 */
const categoryOptions = computed(() => {
  const set = new Set()
  for (const t of tutorials.value) {
    const cat = (t.category || '').trim()
    if (cat) set.add(cat)
  }
  return Array.from(set).sort((a, b) => a.localeCompare(b, 'zh-CN'))
})

/** Markdown 实时预览（与用户端 TutorialDetail 同一渲染链：marked + DOMPurify）。 */
const previewHtml = computed(() => {
  if (!form.value.content?.trim()) return '<p class="tut-preview-empty">（暂无内容，先输入再点预览）</p>'
  return DOMPurify.sanitize(marked.parse(form.value.content))
})


const openNew = () => {
  editing.value = {}
  // 分类留空：不预填「默认」，避免下拉列表里出现一个根本不存在的分类（用户 2026-11-06 定）
  form.value = { title: '', category: '', content: '', sort: 0, enabled: true }
}

const openEdit = (t) => {
  editing.value = t
  form.value = { title: t.title, category: t.category || '默认', content: t.content, sort: t.sort, enabled: t.enabled }
}

const cancel = () => {
  editing.value = null
}

const save = async () => {
  if (!form.value.title.trim()) { toast.error('请输入标题'); return }
  // 分类必填（用户 2026-11-06 定）：不填不允许保存，防止全部落进「默认」组
  if (!form.value.category.trim()) { toast.error('请填写分类（可从下拉选已有分类，或输入新分类）'); return }
  if (!form.value.content.trim()) { toast.error('请输入内容'); return }
  saving.value = true
  try {
    if (editing.value.id) {
      await adminApi.updateTutorial(editing.value.id, form.value)
      toast.success('已更新')
    } else {
      await adminApi.createTutorial(form.value)
      toast.success('已创建')
    }
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段），再回列表
    saved.value = true
    setTimeout(() => {
      saved.value = false
      editing.value = null
    }, 900)
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

/**
 * 把一组教程按当前数组顺序重写 sort（10/20/30…），只传 sort 不碰其他字段。
 * 失败的逐条容忍（某条失败不阻断其余），最后统一刷新 + 汇报。
 */
const persistOrder = async (items) => {
  reordering.value = true
  let failed = 0
  try {
    await Promise.all(items.map((t, i) =>
      adminApi.updateTutorial(t.id, { sort: (i + 1) * 10 })
        .catch(() => { failed++ })
    ))
    await load()
    if (failed) toast.error(`${failed} 篇保存失败，请重试`)
  } finally {
    reordering.value = false
  }
}

/** 组内拖拽落定：把被拖项从原位置挪到目标位置，重写整组 sort。 */
const onDrop = async (group) => {
  const fromId = draggingId.value
  const toId = overId.value
  draggingId.value = null
  overId.value = null
  if (!fromId || !toId || fromId === toId) return

  const list = group.items.slice()
  const fromIdx = list.findIndex((t) => t.id === fromId)
  const toIdx = list.findIndex((t) => t.id === toId)
  if (fromIdx < 0 || toIdx < 0) return
  const [moved] = list.splice(fromIdx, 1)
  list.splice(toIdx, 0, moved)
  await persistOrder(list)
}

/** 字母排序：组内按标题 localeCompare（zh-CN）重写 sort——用户端顺序同步变。 */
const sortByTitle = async (group) => {
  const sorted = group.items.slice().sort((a, b) => a.title.localeCompare(b.title, 'zh-CN'))
  await persistOrder(sorted)
}

const remove = async (t) => {
  if (!confirm(`确定删除「${t.title}」？`)) return
  try {
    await adminApi.deleteTutorial(t.id)
    // 撤销：按原标题/内容重建（教程是简单数据，可恢复）
    toast.success(`已删除「${t.title}」`, 4000, async () => {
      try {
        await adminApi.createTutorial({
          title: t.title,
          category: t.category || '默认',
          content: t.content,
          sort: t.sort ?? 0,
          enabled: t.enabled !== false,
        })
        await load()
        toast.success('已恢复')
      } catch (e) {
        toast.error(e.message || '恢复失败')
      }
    })
    await load()
  } catch (e) {
    toast.error(e.message || '删除失败')
  }
}

const toggleEnabled = async (t) => {
  try {
    await adminApi.updateTutorial(t.id, { enabled: !t.enabled })
    t.enabled = !t.enabled
  } catch (e) {
    toast.error('操作失败')
  }
}

/* ── 拖拽事件（HTML5 原生，不加依赖）── */
const onDragStart = (t) => { draggingId.value = t.id }
const onDragEnter = (t) => { overId.value = t.id }
const onDragEnd = () => { draggingId.value = null; overId.value = null }

/* ── 分类组（大标题）拖拽：整组移动组间顺序 ──
   分类没有独立排序字段：组间序 = 各组「组内最小 sort」。
   拖动后按新组顺序重新分配组基线（100, 200, 300…），组内相对顺序不变。 */
const draggingGroup = ref(null)
const overGroup = ref(null)

const onGroupDragStart = (g) => { draggingGroup.value = g.category }
const onGroupDragEnter = (g) => { overGroup.value = g.category }
const onGroupDragEnd = () => { draggingGroup.value = null; overGroup.value = null }

const onGroupDrop = async () => {
  const from = draggingGroup.value
  const to = overGroup.value
  draggingGroup.value = null
  overGroup.value = null
  if (!from || !to || from === to) return

  const cats = grouped.value.map((g) => g.category)
  const fromIdx = cats.indexOf(from)
  const toIdx = cats.indexOf(to)
  if (fromIdx < 0 || toIdx < 0) return
  cats.splice(toIdx, 0, cats.splice(fromIdx, 1)[0])

  await applyGroupOrder(cats)
}

/** 按给定分类顺序重写 sort：组基线 100/200/300…，组内保持现有相对顺序。 */
const applyGroupOrder = async (cats) => {
  reordering.value = true
  let failed = 0
  try {
    const ops = []
    cats.forEach((cat, gi) => {
      const group = grouped.value.find((g) => g.category === cat)
      group.items.forEach((t, i) => {
        const newSort = (gi + 1) * 100 + (i + 1) * 10
        if (t.sort !== newSort) {
          ops.push(adminApi.updateTutorial(t.id, { sort: newSort }).catch(() => { failed++ }))
        }
      })
    })
    await Promise.all(ops)
    await load()
    if (failed) toast.error(`${failed} 篇保存失败，请重试`)
  } finally {
    reordering.value = false
  }
}

/* ── 分类排序面板（手机友好）：列表内编辑序号 或 A→Z 一键 ── */
const groupSortOpen = ref(false)
/** 面板内的组序编辑副本 [{category, order}]，order 越小越靠前 */
const groupSortItems = ref([])

const openGroupSort = () => {
  groupSortItems.value = grouped.value.map((g, i) => ({ category: g.category, order: i + 1 }))
  groupSortOpen.value = true
}

/** 面板内上下移动一行。 */
const moveGroupSort = (i, dir) => {
  const j = i + dir
  if (j < 0 || j >= groupSortItems.value.length) return
  const list = groupSortItems.value
  ;[list[i], list[j]] = [list[j], list[i]]
}

/** 面板内序号输入后：按新序号重排（同号按原顺序稳定排序）。 */
const onGroupOrderInput = () => {
  groupSortItems.value = groupSortItems.value
    .slice()
    .sort((a, b) => (a.order || 0) - (b.order || 0))
}

/** 面板内 A→Z（按分类名拼音）。 */
const sortGroupByTitle = () => {
  groupSortItems.value = groupSortItems.value
    .slice()
    .sort((a, b) => a.category.localeCompare(b.category, 'zh-CN'))
}

/** 应用面板顺序：转成分类名数组走 applyGroupOrder，成功后关面板。 */
const saveGroupSort = async () => {
  const cats = groupSortItems.value.map((it) => it.category)
  await applyGroupOrder(cats)
  groupSortOpen.value = false
}

onMounted(load)
onActivated(load)
</script>

<template>
  <div class="tut">
    <!-- 列表 -->
    <template v-if="!editing">
      <div class="tut-header">
        <div class="tut-header-left">
          <button class="tut-add-btn" @click="openGroupSort"><Icon name="edit" :size="14" /> 分类排序</button>
          <button class="tut-add-btn" @click="openNew"><Icon name="edit" :size="14" /> 新建教程</button>
        </div>
      </div>

      <!-- 按分类分组：组间按分类名拼音升序，组内按 sort 升序（见 computed grouped）。
           组内条目可拖拽排序（HTML5 原生，落定即写 sort）；
           分类大标题（组）本身也可拖拽排序——拖组标题行整组移动，写回每组第一篇的 sort 维度（组间序）。
           组间序的实现：分类无独立字段，以组内最小 sort 归一化重写全组（组间拖动后整组 sort 平移）。 -->
      <div v-if="grouped.length" class="tut-groups" :class="{ 'is-reordering': reordering }">
        <section
          v-for="g in grouped"
          :key="g.category"
          class="tut-group"
          :class="{ 'is-dragging': draggingGroup === g.category, 'is-over': overGroup === g.category && draggingGroup && draggingGroup !== g.category }"
          draggable="true"
          @dragstart="onGroupDragStart(g)"
          @dragenter.prevent="onGroupDragEnter(g)"
          @dragover.prevent
          @drop.prevent="onGroupDrop"
          @dragend="onGroupDragEnd"
        >
          <header class="tut-group-head">
            <span class="tut-drag-handle" aria-hidden="true">⠿</span>
            <span class="tut-group-name">{{ g.category }}</span>
            <span class="tut-group-count">{{ g.items.length }} 篇</span>
            <button
              class="tut-sort-btn"
              title="按标题字母/拼音排序（用户端顺序同步变）"
              :disabled="reordering"
              @click.stop="sortByTitle(g)"
            >A→Z</button>
          </header>
          <div class="tut-list">
            <div
              v-for="t in g.items"
              :key="t.id"
              class="tut-item"
              :class="{ 'is-dragging': draggingId === t.id, 'is-over': overId === t.id && draggingId !== t.id }"
              draggable="true"
              @dragstart="onDragStart(t)"
              @dragenter.prevent="onDragEnter(t)"
              @dragover.prevent
              @drop.prevent="onDrop(g)"
              @dragend="onDragEnd"
            >
              <div class="tut-item-info">
                <span class="tut-drag-handle" aria-hidden="true">⠿</span>
                <span class="tut-item-title">{{ t.title }}</span>
                <span class="tut-item-sort">排序: {{ t.sort }}</span>
              </div>
              <div class="tut-item-actions">
                <button class="tut-toggle" :class="{ 'tut-toggle--on': t.enabled }" @click="toggleEnabled(t)">
                  {{ t.enabled ? '已启用' : '已禁用' }}
                </button>
                <button class="tut-icon-btn" @click="openEdit(t)"><Icon name="edit" :size="16" /></button>
                <button class="tut-icon-btn tut-icon-btn--danger" @click="remove(t)"><Icon name="trash" :size="16" /></button>
              </div>
            </div>
          </div>
        </section>
      </div>
      <p v-else class="tut-empty">暂无教程</p>
    </template>

    <!-- 编辑：返回在左、标题在右（与用户端二级页头部方向一致） -->
    <template v-else>
      <div class="tut-header">
        <button class="tut-back-btn" @click="cancel"><Icon name="chevron-left" :size="14" /> 返回</button>
        <h2 class="tut-title">{{ editing.id ? '编辑教程' : '新建教程' }}</h2>
      </div>

      <div class="tut-form">
        <div class="tut-field-row2">
          <div class="tut-field tut-field--grow">
            <label class="tut-label">标题</label>
            <input v-model="form.title" class="tut-input" placeholder="如：Clash 使用教程" />
          </div>
          <div class="tut-field tut-field--cat">
            <label class="tut-label">分类（可选已有或输入新的，必填）</label>
            <input v-model="form.category" class="tut-input" list="tut-category-options" placeholder="如：安卓系列" />
            <datalist id="tut-category-options">
              <option v-for="c in categoryOptions" :key="c" :value="c" />
            </datalist>
          </div>
        </div>
        <div class="tut-field">
          <label class="tut-label">组内排序（数字越小越靠前；手机上改这个数字即可调组内顺序）</label>
          <input v-model.number="form.sort" type="number" class="tut-input tut-input--sm" />
        </div>
        <div class="tut-field">
          <div class="tut-content-head">
            <label class="tut-label">内容（Markdown）</label>
            <button
              type="button"
              class="tut-preview-btn"
              :class="{ 'is-active': previewMode }"
              @click="previewMode = !previewMode"
            >{{ previewMode ? '← 返回编辑' : '预览' }}</button>
          </div>
          <textarea v-if="!previewMode" v-model="form.content" class="tut-textarea" rows="20" placeholder="# 教程标题&#10;&#10;正文内容..." />
          <div v-else class="tut-preview tut-preview--full" v-html="previewHtml"></div>
        </div>
        <div class="tut-field tut-field--row">
          <label class="tut-label">启用</label>
          <label class="tut-switch">
            <input type="checkbox" v-model="form.enabled" />
            <span class="tut-switch-slider"></span>
          </label>
        </div>
        <div class="tut-actions">
          <button class="tut-save-btn" :class="{ 'is-success': saved }" :disabled="saving || saved" @click="save">
            <span v-if="saved" class="tut-save-check" aria-hidden="true">✓</span>
            {{ saving ? '保存中...' : saved ? '已保存' : '保存' }}
          </button>
          <button class="tut-cancel-btn" @click="cancel">取消</button>
        </div>
      </div>
    </template>

    <!-- 分类排序弹层（居中卡片 + 遮罩，手机左右贴 12px 边；Teleport 到 body 不受页面滚动影响） -->
    <Teleport to="body">
      <div v-if="groupSortOpen" class="tut-gsort-mask" @click.self="groupSortOpen = false">
        <div class="tut-gsort">
          <div class="tut-gsort-head">
            <span class="tut-gsort-title">分类排序（数字越小越靠前）</span>
            <button class="tut-sort-btn" @click="sortGroupByTitle">A→Z</button>
          </div>
          <div v-for="(it, i) in groupSortItems" :key="it.category" class="tut-gsort-row">
            <input
              v-model.number="it.order"
              type="number"
              class="tut-input tut-input--sm tut-gsort-order"
              @change="onGroupOrderInput"
            />
            <span class="tut-gsort-name">{{ it.category }}</span>
            <button class="tut-gsort-move" :disabled="i === 0" @click="moveGroupSort(i, -1)">↑</button>
            <button class="tut-gsort-move" :disabled="i === groupSortItems.length - 1" @click="moveGroupSort(i, 1)">↓</button>
          </div>
          <div class="tut-gsort-actions">
            <button class="tut-cancel-btn" @click="groupSortOpen = false">取消</button>
            <button class="tut-save-btn" :disabled="reordering" @click="saveGroupSort">
              {{ reordering ? '保存中...' : '保存分类顺序' }}
            </button>
          </div>
        </div>
      </div>
    </Teleport>
  </div>
</template>

<style scoped src="@/assets/styles/pages/admin/Tutorials.css"></style>
