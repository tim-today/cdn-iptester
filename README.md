# CDN-IPtester

`CDN-IPtester` 是一个基于 Go 语言与 Wails 框架构建的高性能 CDN (如 Cloudflare 等) IP 优选与测速测试工具。它提供了极简现代的暗色自适应可视化操作界面，支持多网段自定义管理、多选测速、反代 IP 优选以及测速结果一键同步到 Cloudflare Worker。

---

## 🚀 核心功能

*   **配置界面极简化**：默认仅显示最核心的 `Worker 地址` 与 `Worker 密码`。其他 20+ 项高阶测速/反代参数支持“高级配置”按钮一键折叠展开，清爽高效。
*   **IP 段来源卡片式管理**：
    *   移除硬编码，改由本地 `ip_sources.json` 动态持久化驱动。
    *   内置卡片式布局，清晰显示每个 IP 段的本地缓存状态和**上次更新时间**。
    *   提供一键物理缓存下载与本地预览网段内容（Modal 弹窗形式）功能，支持直接对 IP 段进行增加、删除和编辑。
*   **IP 优选与多选测速**：支持在测速时**多选勾选**本地已缓存的 IP 网段，后台合并去重并拉起多协程测速，精准筛选可用入口 IP。
*   **反代 IP 优选**：支持根据所选国家/地区自动筛选并优选反代 IP 结果，并在前端汇总展示及复制。
*   **同步到 CF**：测速完成后支持一键将优选结果 (`CDN-IPtester.txt`) 或是反代 IP 结果 (`PROXYIP-tester.txt`) 同步推送到你的 Cloudflare Worker 中。
*   **200ms TCP 代理探针**：为网段更新前置了局域网代理 TCP 拨号检测。如果本地代理不在线，在 **200ms 内** 快速失败并秒切直连下载，彻底解决因代理离线握手过长导致的前端卡顿。
*   **完美适配暗色模式**：前台样式全面重构，自适应操作系统的深浅外观，完美解决白底白字对比度缺陷，修护了多选框错位排版。

---

## 🛠️ 快速开始

### 前提条件
*   安装 [Go](https://go.dev/) (推荐 1.20 或以上版本)
*   安装 [Wails](https://wails.io/) 命令行工具 (用于开发与构建桌面应用)

### 本地开发
1. 克隆本项目仓库：
   ```bash
   git clone https://github.com/tim-today/cdn-iptester.git
   cd cdn-iptester
   ```
2. 启动本地开发热重载服务器：
   ```bash
   wails dev
   ```

### 编译构建
编译生成适配你当前系统（macOS / Windows）的独立绿色版安装包或可执行程序：
```bash
wails build
```

---

## 📂 文件产物说明

*   `CDN-IPtester.txt`：IP 测速优选出的最佳入口 IP 结果列表。
*   `PROXYIP-tester.txt`：反代 IP 优选出的最佳反代 IP 结果列表。
*   `configs/config.yaml`：可视化编辑同步修改的项目全局配置文件。
*   `configs/ip_sources.json`：持久化存储 IP 网段来源列表的文件。
