# 第三方组件与许可声明

本项目部分代码与资源来源于以下第三方开源项目，谨此致谢并保留其版权声明。

## 节点监控 · 地球可视化

- **来源**：[komari-theme-aero](https://github.com/biliblihuorong/komari-theme-aero)
- **许可**：MIT License，Copyright (c) 2026 Shell (biliblihuorong)
- **使用范围**：前端「节点地区」地球的绘制实现参考并移植自该主题的 `globe.js`
  （正交投影、陆地点阵渲染、拖拽 / 点击交互与在线脉冲动画），已按本项目组件体系
  重写封装为 `frontend/src/components/admin/NodeGlobe.vue` 与 `frontend/src/utils/globe.js`，
  配色改为本项目设计 token。
- **许可全文**：<https://github.com/biliblihuorong/komari-theme-aero/blob/main/LICENSE>

## 地图数据

- `frontend/src/assets/earth.json`（陆地点阵与地区中心坐标）基于
  [Natural Earth](https://www.naturalearthdata.com/) 数据（公有领域，无需署名）。
