# 全局样式目录规范（src/assets/styles）

> 目标：**按用途归类到不同文件夹**，组件/页面的样式与 `.vue` 分离，方便集中查找、复用与维护。

## 目录结构

```
src/assets/styles/
├── base/                设计变量 + 全局基类（必须先于其它加载）
│   ├── tokens.css       CSS 变量（颜色/间距/字体/圆角/动效）
│   ├── main.css         Tailwind 指令 + 重置 + 全局通用基类（.admin-table 等）
│   ├── app.css          App.vue 的全局基础样式（html/body/#app、启动动画）
│   └── user-ui.css      用户端全局主题变量与基础壳（原 styles/user/user-ui.css）
├── shared/              跨页公共类（全局，非 scoped）
│   ├── table.css        .ch-table 公共数据表格类（13 页复用）
│   └── common.css       跨页通用类：.ch-page-title / .ch-form / .ch-field /
│                        .ch-empty / .ch-actions / .ch-desc 等（由各页重复规则合并而来）
├── pages/               各「页面」的 scoped 样式（对应 src/views）
│   ├── admin/           admin/*.vue 的样式，文件名与组件同名
│   └── user/            user/*.vue 的样式
├── components/          各「组件」的 scoped 样式（对应 src/components）
│   ├── ui/              ui/*.vue（Button/Input/Modal…）
│   ├── admin/           admin/*.vue
│   └── user/            user/*.vue
└── layouts/             layouts/*.vue（AdminLayout / UserLayout）
```

组件/页面里用别名引用（构建器支持 `<style scoped src>` 的 `@` 别名）：

```vue
<style scoped src="@/assets/styles/pages/admin/Dashboard.css"></style>
```

## 分类规则

| 源 `.vue` 位置 | 样式放哪 |
|---|---|
| `views/admin/*.vue` | `pages/admin/` |
| `views/user/*.vue` | `pages/user/` |
| `components/ui/*.vue` | `components/ui/` |
| `components/admin/*.vue` | `components/admin/` |
| `components/user/*.vue` | `components/user/` |
| `layouts/*.vue` | `layouts/` |

> 样式文件名与 `.vue` 同名；`pages/admin/Dashboard.css` 与 `pages/user/Dashboard.css` 靠目录区分，不会冲突。

## 硬规矩

> 适用范围：**任何人（含 AI 助手）改前端样式都必须遵守本规范，不接受绕过。**
> 改任何样式前先读本文件；新增/修改后在 PR/提交说明里注明归类依据。

0. **样式与 `.vue` 分离**：一律抽到 `pages/` 或 `components/` 分类目录，用 `<style scoped src="@/...">` 引入；
   只有「跨页复用」的类才进 `shared/`，**单页专属样式留在本页 css**（如 `pages/user/Orders.css` 的 `.od-*`）。
   **颜色/间距/圆角只用 `base/tokens.css` 的语义变量**（`--accent/--lbl1/--fill3/--sep/--radius-*`…），
   **禁止硬编码 hex/rgba/px 魔法值**（新样式至少复用既有 token，缺变量先在 tokens 里加）。
1. **加载顺序**：`base/tokens.css` → `base/main.css`（`src/main.js` 已固定），`shared/*` 次之。
2. **scoped 样式**放对应 `pages/` 或 `components/` 分类目录，用 `<style scoped src="@/...">`；不再写进 `.vue`。
3. **跨页复用**的样式才进 `shared/`（如 `.ch-table`、`.ch-form`）；单页特有样式留在该页自己的 css。
4. ⚠️ **改/加 Tailwind 工具类要注意**：`tailwind.config.js` 的 `content` 必须包含 `css`
   （`src/**/*.{vue,js,css}`），否则写在抽出的 `.css` 里的 Tailwind 类不会被生成。
5. **改全局 css 验证零回归**：`npx vite build` 后对比产物 CSS（归一化 `data-v-` 与 keyframes 哈希），
   规则集合不应有丢失。
6. ⚠️ **把 scoped 类改成全局共享类时，必须查特异性**：scoped 规则带 `[data-v-x]`（优先级更高），
   改成全局后优先级降低，可能与「同一元素上其它类的规则」发生翻转。
   - 必查：同一元素上的修饰类（如 `.xx-field--row`），若它在原文件里**写在基类之前**且**声明属性冲突**，
     合并后行为会变 → 该页**不要合并**（Tutorials 的 `.tut-field-row2` 就是这种情况，已保留原样）。
   - 已有排查脚本思路：对每个待合并类，取其原规则位置 + 同元素其它类的规则位置与属性，找 `spos < bpos` 且属性交集非空者。

## 手机端留白约定（用户定稿）

- 后台内容区窄屏已贴边（`AdminLayout` 的 `.al-content { padding: 0 }`），因此**卡片内边距就是唯一留白**。
- `.ch-card` 窄屏（≤767px）内边距收到 **12px / 16px**、圆角降一档，避免「内容区 0 + 卡片 24」两层留白
  在 375px 屏上浪费近 50px 宽度。
- **手机不要多重方块、多重边距**：新增样式时不要在窄屏再给容器叠 padding/margin。

## 安全迁移流程（已验证）

抽取 `<style scoped>` → 独立 css 时：构建对比「抽取前 vs 抽取后」的 CSS 规则集合，应**0 丢失**；
只允许出现由 `data-v`/keyframes 后缀哈希变化带来的等价差异，以及 Tailwind 补齐类。
