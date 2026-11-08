import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'
import path from 'node:path'

// https://vite.dev/config/
export default defineConfig({
  plugins: [vue()],
  build: {
    // 构建产物直接落到 server 的 embed 目录：server 用 go:embed 打包 dist，
    // 编译出的二进制自带前端，服务器上不用再单独放静态文件。
    // outDir 在项目根之外，vite 默认拒绝清空它（会残留上一版带 hash 的旧 chunk），
    // 所以必须显式 emptyOutDir。
    outDir: path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../server/internal/web/dist'),
    emptyOutDir: true,
    // ===== CSS / JS 构建目标（必须显式指定，不要用默认）=====
    //
    // 原因（实际踩坑）：Vite 8 + rolldown 默认会把 `@media (max-width: 640px)`
    // 压缩成 CSS Media Queries Level 4 的区间语法 `(width<=640px)`。
    // 该语法 Safari 16.4 / Chrome 104 以下**完全不支持** —— 老手机（尤其 iOS）
    // 上整段响应式规则静默失效：卡片不会收窄、表格超宽被 overflow:hidden 裁掉、
    // 右侧列看不见也滑不动。
    //
    // 指定 safari15 / chrome87 后，lightningcss 会退回传统写法
    // `@media (max-width:640px)`，覆盖到绝大多数在用设备。
    cssTarget: ['safari15', 'chrome87', 'firefox78', 'edge88'],
    target: ['es2020', 'safari15', 'chrome87', 'firefox78', 'edge88'],
    rollupOptions: {
      output: {
        // ===== vendor 分块策略（为长期发展：按库拆分，而不是全塞进入口）=====
        //
        // 目的：
        //   1) 框架/工具库独立成 chunk，改业务代码时它们的 hash 不变 —— 老用户
        //      只需重下业务 chunk，vendor 走缓存；
        //   2) 重库按用途分块，只随用到它的路由加载，不会进首屏；
        //   3) 以后多个路由共用同一个库时自动复用同一 chunk，不会再各自打一份。
        //
        // 现状标注（谁在用）：
        //   vue / vue-router / pinia —— 全站
        //   axios                    —— api/client.js（全部接口调用）
        //   gsap                     —— 仅 views/user/Home.vue（落地页动画）
        //   qrcode                   —— 仅 components/user/QrSheet.vue（订阅二维码）
        //   marked                   —— 仅教程页（Tutorials / TutorialDetail）
        //   dompurify                —— CustomHome（落地页用）+ 教程页
        //
        // ⚠️ marked 与 dompurify 必须**分开**成组：落地页 CustomHome 只用 dompurify，
        // 若合成一个 chunk，首屏会被迫多下载整个 marked（实测 +12KB gzip）。
        //
        // priority 越大越先匹配；未命中的模块仍由 rolldown 自动按路由分块。
        codeSplitting: {
          groups: [
            { name: 'vendor-core', test: /node_modules[\\/](?:@vue[\\/]|vue[\\/]|vue-router[\\/]|pinia[\\/])/, priority: 100 },
            { name: 'vendor-axios', test: /node_modules[\\/]axios[\\/]/, priority: 90 },
            { name: 'vendor-gsap', test: /node_modules[\\/]gsap[\\/]/, priority: 80 },
            { name: 'vendor-qrcode', test: /node_modules[\\/](?:qrcode[\\/]|dijkstrajs[\\/]|pngjs[\\/]|encode-utf8[\\/])/, priority: 80 },
            { name: 'vendor-marked', test: /node_modules[\\/]marked[\\/]/, priority: 80 },
            { name: 'vendor-dompurify', test: /node_modules[\\/]dompurify[\\/]/, priority: 80 },
          ],
        },
      },
    },
  },
  resolve: {
    alias: {
      '@': path.resolve(path.dirname(fileURLToPath(import.meta.url)), 'src'),
    },
  },
  server: {
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: true,
        // 本地模拟节点的域名不可达，后端逐台探测要等超时，默认代理超时会 502
        timeout: 120000,
      },
      '/admin-api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: true,
        timeout: 120000,
      },
      '/storage': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: true,
      },
    },
  },
})
