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

// 分组与排序口径（与管理端「分类排序」的存储语义一致）：
//   管理端保存分类顺序时写入 sort = (组序)*100 + (组内位置)*10，
//   即**组间顺序编码在 sort 里**（第1组 110/120…、第2组 210/220…）。
//   因此：组内按 sort 升序 + id 兜底；组间按「组内最小 sort」升序。
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
