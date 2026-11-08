import { createApp } from 'vue'
import App from './App.vue'
import router from '@/router'
import pinia from '@/stores'
import { useThemeStore } from '@/stores/theme'

// Design tokens (must load before Tailwind directives in components).
import './assets/styles/base/tokens.css'
// Tailwind base / components / utilities.
import './assets/styles/base/main.css'
// 跨页公共样式（表格等复用类）。
import './assets/styles/shared/table.css'
// 跨页公共样式（表单/字段/空状态/标题/操作区等复用类）。
import './assets/styles/shared/common.css'

const app = createApp(App)
app.use(router)
app.use(pinia)

// 应用持久化的主题（index.html 内联脚本已预置类避免闪烁，这里同步 store 状态）
useThemeStore().init()

app.mount('#app')
