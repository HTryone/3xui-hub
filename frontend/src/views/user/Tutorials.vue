<script setup>
/**
 * Tutorials — 使用文档全屏页（二级页面）。
 * 数据来自 userApi.tutorials()（GET /api/tutorials，后端已按 sort 升序）。
 */
import { ref, onMounted, onActivated } from 'vue'
import { useRouter } from 'vue-router'
import { userApi } from '@/api/user'
import ListGroup from '@/components/user/ListGroup.vue'
import ListRow from '@/components/user/ListRow.vue'
import UserIcon from '@/components/user/UserIcon.vue'

const router = useRouter()
const docs = ref([])

const fmtDate = (s) => {
  const d = new Date(s)
  if (Number.isNaN(d.getTime())) return ''
  return d.toLocaleDateString('zh-CN', { year: 'numeric', month: 'long', day: 'numeric' })
}

// 分组与排序口径：
//   - 组内：按 sort 升序、id 兜底（后台「组内拖拽 / 字母排序」改的就是 sort 字段）
//   - 组间：按「组内最小 sort」升序 —— 管理员设的 sort 是全局优先级，
//     谁排在最前面，它的分类就排在最前面（而不是按分类名字母序，那会打乱管理员的安排）
const groups = ref([])
const groupDocs = (list) => {
  const map = new Map()
  for (const t of list) {
    const cat = (t.category || '').trim() || '默认'
    if (!map.has(cat)) map.set(cat, [])
    map.get(cat).push(t)
  }
  return [...map.entries()]
    .map(([label, items]) => {
      const sorted = items.slice().sort((a, b) => (a.sort ?? 0) - (b.sort ?? 0) || (a.id ?? 0) - (b.id ?? 0))
      return { label, items: sorted, minSort: sorted.length ? (sorted[0].sort ?? 0) : 0 }
    })
    .sort((a, b) => a.minSort - b.minSort)
}

/** 拉取并分组（供 onMounted 与「回到前台」复用）。 */
const load = async () => {
  try { docs.value = (await userApi.tutorials()) || [] } catch (e) { docs.value = [] }
  groups.value = groupDocs(docs.value)
}

const openDoc = (id) => router.push(`/dashboard/tutorial/${id}`)

onMounted(load)
// keep-alive 回到本页时重新拉一次：管理员刚改过排序/分类，用户端不该还是旧顺序。
onActivated(load)
</script>

<template>
  <div class="td uu-slide-up">
    <template v-if="groups.length">
      <ListGroup
        v-for="g in groups"
        :id="'tut-cat-' + encodeURIComponent(g.label)"
        :key="g.label"
        :label="g.label"
      >
        <ListRow
          v-for="(t, i) in g.items"
          :key="t.id"
          :label="t.title"
          :sub="fmtDate(t.created_at)"
          :last="i === g.items.length - 1"
          @press="openDoc(t.id)"
        >
          <template #icon><UserIcon name="doc" :size="20" /></template>
        </ListRow>
      </ListGroup>
    </template>
    <ListGroup v-else label="使用文档">
      <p class="td-empty">暂无文档，敬请期待。</p>
    </ListGroup>
  </div>
</template>

<style scoped src="@/assets/styles/pages/user/Tutorials.css"></style>
