# Limpet

简体中文 | [English](./README.en.md) | [For AI Agents](./README.AGENT.md)

一个轻量、单用户的持久化 Web + 本地 CLI 终端宿主，专为 Linux 与 WSL2（如在手机或平板上远程盯跑 AI coding agent、终端构建与调试会话）设计。

单个 Go 二进制内置基于 [`libghostty-vt`](https://github.com/ghostty-org/ghostty) 的终端网格与内嵌前端页面（Vue 3 + `@wterm/vue` + `@wterm/ghostty`），零数据库、零外部字体或运行时服务依赖。

---

## 核心特性

- **会话独立存活（不随 server 掉线）**：每个终端由独立的 `worker` 进程持有 PTY 与双屏幕 VT 网格，支持重启或升级 `limpet-server` 而不断开正在跑的会话。
- **本地 CLI + Web 多端同屏**：在本地 WSL/Linux 直接运行 `limpet` 或 `limpet codex --yolo` 等命令，关闭本地终端标签页后会话继续在后台运行，可随时用手机/桌面浏览器或 `limpet attach` 接回。
- **锁定原宽（不抢 PC 宽度）**：手机端可一键切换「锁定原宽」——查看和发送输入均按 PC / CLI 当下的真实 `cols×rows` 渲染（支持横纵平滑滚动与独立横滑轨道），不再因为手机竖屏窄把电脑端 TUI 框线挤坏。
- **移动端顺手操作与 PWA**：
  - 统一暗黑低对比度护眼界面，支持 PWA 添加到主屏幕独立全屏运行（`standalone`）；
  - 可调终端字号（`10–18px`，按端自适应默认值并持久化）；
  - 单行可横滑快捷键条（`Esc`、`Ctrl+C`、`Tab`、方向键、`Enter`、粘性 `Ctrl`）+ 固定尾端开关的可展开中文九宫格 / 语音输入条（IME 安全提交）；
  - 桌面宽屏（`≥1024px`）自动切换双栏工作区，移动端支持顶栏快速切换活跃会话；
  - 断线自动重连、无声失联心跳检测、状态识别与精确重放（断线期间不丢字也不重复）。

---

## 安全边界说明

Limpet 本身**不包含用户登录与鉴权逻辑**，默认仅监听 `127.0.0.1`：

- 严禁直接把未加认证的 Limpet 端口暴露到公网或不可信局域网。
- 远程访问请前置 **Cloudflare Tunnel + Cloudflare Access**、Tailscale / WireGuard 或带前置鉴权的反向代理（前端已针对 Cloudflare Access 重定向与带凭据拉取 `manifest.json` 做好适配）。

---

## 环境要求

- **系统**：Linux 或 WSL2（正式服务托管依赖 `systemd --user` 及 `linger`）
- **后端构建**：Go（开启 cgo）、`gcc` / C 工具链、`pkg-config`、[`libghostty-vt`](https://github.com/ghostty-org/ghostty) 头文件与库
- **前端构建**：Node.js + `pnpm`（仅编译期使用，构建后产物内嵌进 Go 二进制）

---

## 快速开始

### 1. 本地开发 / 试跑（不注册系统服务）

确保 `pkg-config` 能找到 `libghostty-vt`（若安装在自定义路径，先设置 `PKG_CONFIG_PATH`）：

```sh
# 1. 构建前端产物到 web/dist
cd web && pnpm install --frozen-lockfile && pnpm build && cd ..

# 2. 构建内嵌前端的单二进制（带 limpet_embed tag）
PKG_CONFIG_PATH="${LIMPET_PKG_CONFIG:-$HOME/.local/opt/limpet-toolchain/install-1.0.1/usr/lib/pkgconfig}${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}" \
  go build -tags limpet_embed -o ~/.limpet-dev/limpet-server ./server
ln -sfn ~/.limpet-dev/limpet-server ~/.limpet-dev/limpet

# 3. 用独立数据目录启动本地 server（默认监听 127.0.0.1:4040）
LIMPET_HOME=~/.limpet-dev ~/.limpet-dev/limpet-server
```

打开浏览器访问 `http://127.0.0.1:4040`。在另一个终端里用本地 CLI 接入同一个开发 server：

```sh
LIMPET_HOME=~/.limpet-dev ~/.limpet-dev/limpet zsh
```

> **提示**：开发/测试务必通过 `LIMPET_HOME` 与正式服务默认目录 `~/.limpet` 隔离，不要让两个 server 共用同一份 `sessions.json`。

### 2. 正式部署（systemd 用户服务）

详细安装、端口配置、环境变量同步、升级与卸载步骤见 [deploy/README.md](./deploy/README.md)。核心只需两步：

```sh
# 首次创建安装目录（仅一次）
sudo mkdir -p /opt/limpet && sudo chown "$USER": /opt/limpet

# 构建并安装到 /opt/limpet，生成 ~/.local/bin/limpet 软链并启动 127.0.0.1:13000（可用 LIMPET_PORT 自定义）
./deploy/install.sh
```

> 默认生产监听端口为 `127.0.0.1:13000`，可通过 `LIMPET_PORT=<端口> ./deploy/install.sh` 调整（开发直接运行 `limpet-server` 时默认 `127.0.0.1:4040`，可通过 `LIMPET_ADDR` 修改）。执行 `deploy/install.sh` 会直接启用并启动正式服务，请先确认下方反向代理 / Cloudflare Access 已配置好。

---

## 远程访问：Cloudflare Tunnel + Access 配置

推荐通过 Cloudflare Zero Trust 暴露服务，无需开放公网端口或维护反向代理证书：

1. **配置 Cloudflare Tunnel（`cloudflared`）**：
   - 在 Cloudflare Zero Trust 创建 Tunnel，添加一条 Public Hostname（如 `terminal.example.com`），Service 指向 `HTTP` → `http://127.0.0.1:13000`（若安装了自定义 `LIMPET_PORT`，这里填写对应端口）。
   - Cloudflare Tunnel 原生透传 WebSocket 升级请求，无需额外开关。
2. **配置 Cloudflare Access（鉴权门禁）**：
   - 在 **Access → Applications** 新建 **Self-hosted** 应用，域名填同一地址，添加仅允许本人邮箱或指定身份提供商的 **Allow** 策略。
   - 可按习惯调整 **Session Duration**（如 `24h` 或更长），减少手机切回应用时重新验证的次数。
3. **上线前验证拦截是否生效**：
   ```sh
   curl -sI https://<your-domain>/api/term   # 期望返回 302，Location 指向 *.cloudflareaccess.com
   ```
4. **手机安装 PWA（可选 Bypass）**：
   - 首页已对 `/manifest.json` 启用 `crossorigin="use-credentials"`，使浏览器携带 `CF_Authorization` Cookie 拉取清单。
   - 若部分 Android 浏览器在打包桌面 WebAPK 时使用无 Cookie 后台抓取图标导致安装失败，可在 Access 中额外建一条 **Bypass**（Everyone）策略，**仅放行以下 4 个纯静态资源路径**（不含任何会话或接口数据）：
     - `/manifest.json`、`/limpet.svg`、`/limpet-192.png`、`/limpet-512.png`

---

## 本地 CLI 速查

`limpet` 与 `limpet-server` 是同一个二进制，通过可执行文件名分发行为：

| 命令 | 说明 |
| --- | --- |
| `limpet` | 在当前工作目录启动交互式 Shell 并接入 |
| `limpet [-n 名称] [--] <命令> [参数...]` | 启动指定程序并接入（如 `limpet codex --yolo`、`limpet -n api -- npm run dev`） |
| `limpet ls` | 列出当前活跃会话（名称、ID、接入状态、目录、命令） |
| `limpet attach <名称或ID>` | 接回后台运行的已有会话 |
| `limpet server` | 前台启动 HTTP/WebSocket server |
| `limpet version` | 显示编译版本号 |

本地客户端退出或关闭所在终端标签页时，会发送重置序列还原宿主机终端并干净脱离（detach），后台会话与进程继续存活。

---

## 相关文档

- [English README](./README.en.md)
- [AI Agent 指南（部署与操作手册，英文）](./README.AGENT.md)
- [正式安装与运维说明](./deploy/README.md)

## License

[MIT](./LICENSE)
