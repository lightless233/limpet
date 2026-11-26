# Plan 007：Web 视觉重构、PWA 全屏与锁定原宽模式

- 日期：2026-11-26
- 对应 Spec：`docs/specs/007-2026-11-26-web-ui-pwa-and-lock-width.md`
- 执行方式：DELEGATED（已通过 GPT 只读预审并完成 Spec/Plan 修订，按 U1 → U5 顺序实施）
- 约束基线：
  - **零新增外部依赖**：不修改 `go.mod` 与 `web/package.json` 的运行时依赖；不引入外部字体包或 Service Worker 框架；
  - 所有开发与测试均带 `LIMPET_HOME=~/.limpet-dev`；未获用户明确指示严禁执行 `deploy/install.sh` 上线或触碰 `/opt/limpet` 与正式 `~/.limpet`；
  - 临时验证产物（Playwright 截图、临时测试二进制等）放在 `.runtime/` 并在收尾删除，不在仓库残留大体积文件；
  - 按 `AGENTS.md` 每完成一个可提交单元自动 commit（Conventional Commits + `Co-authored-by` 尾注按实际执行的 agent / 模型填写，拿不准先向用户确认，不主动 push）。

## 总体顺序与依赖关系

```text
U1（后端增量：/api/term 白名单投影 / *uint16 0×0 与 v.locked / 同锁 ftReady / 单队列保序 ftSize）
  ├──▶ U2（设计系统基座 CSS、字体、基础 .wterm、PWA manifest.json 与图标）
  │      └──▶ U3（双栏响应壳、cwd/cmd 列表、原生 <dialog> 与 S10 回归迁移）
  │             └──▶ U4（终端页：快切、字号探针、锁定原宽与固定网格滚动、可横滑按键栏与 IME 输入条、S9 迁移）
  │                    └──▶ U5（embed 编译与真响应校验、顺序执行回归脚本、Playwright 双端核验与清理）
```

## 实施单元

### U1：后端最小增量、并发保序与单测更新

- **目标**：在不破坏 Spec 006 既有语义的前提下，安全发布 `cwd`/`cmd` 元数据，精确实现 `(0,0)` 清空与不抢 `s.owner`，并保证 `ftReady` 尺寸和实时 `ftSize` 与 VT 字节流的严格因果序。
- **涉及文件**：`server/proto.go`、`server/session.go`、`server/workerapp.go`、`server/bridge.go`、`server/manager.go`、`server/control_test.go`、`server/bridge_test.go`、`server/workerapp_test.go`。
- **关键改动与防坑点**：
  1. **HTTP 投影层脱敏白名单（`manager.go` + `control_test.go`）**：
     - `manager.list()` 在既有 `{id, name, created, attached}` 基础上，仅当非空时增加 `"cwd"`（在服务端投影时借助 `os.UserHomeDir()` 将家目录前缀折叠为 `~`，不改动 CLI 控制列表与注册表）和 `"cmd"`（仅可执行文件基名）。
     - 在 `control_test.go` 走真实 HTTP `GET /api/term`，分别断言有元数据与无元数据会话的精确字段白名单、`~` 折叠结果，以及真实启动参数中携带的测试标记不会出现在响应中。
  2. **严格 `0×0` 解析与 `v.locked` 所有权边界（`bridge.go` + `workerapp.go`）**：
     - 两侧解码 `resize` 控制 JSON 时使用 `*uint16`（`Cols, Rows *uint16`），要求两个字段都显式存在且非 `nil`，避免 `{ "type": "resize" }` 或 `null` 字段因零值误触发清空；真实尺寸 `valid()` 范围保持不变。
     - `viewer` 增加 `locked bool`（由 `s.inputMu` 保护）：收到显式 `(0,0)` 置 `v.locked = true`、清空 `v.dims`、若 `s.owner == v` 则置 `s.owner = nil`；收到有效尺寸置 `v.locked = false`、更新 `v.dims`、若 `s.owner == v && !v.closed.Load()` 则 `s.resize()`。
     - `ftInput` 在 `s.inputMu` 下分支：若 `v.locked` 为真，仅写 `s.ptmx`（不改 `s.owner`、不调 `s.resize`）；否则保持 Spec 006 完整行为（`if s.cli { s.owner = v }` 及 `if v.dims.valid() { s.resize(...) }`）。
  3. **同锁快照尺寸与单队列 FIFO `ftSize`（`session.go` + `workerapp.go` + `bridge.go`）**：
     - `rebuildFor(v)` 在持有同一把 `s.gridMu` 期间同时取出当前 `curCols, curRows` 返回给 `serveViewer` 填入 `ftReady`（首次无重放接入也附带真实初始尺寸）；`bridge.go` 对空/无效载荷 `ftReady` 仍发出 `{"type":"ready"}`。
     - 将 `viewer.queue` 统一为有界 FIFO `chan viewerEvent`（单条队列按序承载 `ftOutput` 字节块与 `ftSize` 事件）。`handleOutput` 与 `s.resize`（真实变更尺寸后）均向活跃且切点已追上的 viewer 非阻塞入队，队列满时按 `4001` 仅丢弃该慢 viewer。
     - `serveViewer` 保持唯一写者：遇到 `ftSize` 事件前先把当前合批缓存的 `ftOutput` 写出，再写 `ftSize` 帧（`0x07`）；`v.queue` 关闭正常退出时先排空队列剩余尾部事件再写 `ftExit`；`bridge.go` 将 `ftSize` 转为 `{"type":"size","cols":...,"rows":...}`。
  4. **更新已有 Go 回归**：同步更新 `workerapp_test.go` 中 `TestCLIWorkerActiveSize`（当前遇到 `ftSize` 会被当成意外帧失败），使其兼容/校验 `ftSize`。
- **完成证据**：`go test -race -count=1 ./server` 全过；新增单测覆盖：HTTP 有/无元数据响应与 `argv` 标记不泄露、缺字段/`null` 的 `resize` 防误清、手机已是 owner 后切锁清空 owner、CLI 首输重新取得后 `SIGWINCH` 立即生效，以及 CLI 已是 owner 时手机持续输入不影响 CLI 即时 `SIGWINCH`、Spec 006 CLI 首输晋升保留、卡住重放/输出泵时交错 `resize` 与 VT 的帧序与最终网格一致、空载荷旧 `ftReady` 兼容。

### U2：设计系统基础、PWA 清单与全屏视口基础设施

- **目标**：修复全站暗黑主题一致性、字体回退，并为手机“添加到主屏幕”提供可靠 PWA 元数据与图标。
- **涉及文件**：`web/index.html`、`web/src/style.css`、`web/public/manifest.json`、图标资源（`web/public/`）。
- **关键改动与防坑点**：
  1. `web/index.html`：设置 `viewport-fit=cover, interactive-widget=resizes-content`、`theme-color=#121417`、`apple-mobile-web-app-capable`、`<link rel="manifest" href="/manifest.json">` 与 `apple-touch-icon`。
  2. `web/public/manifest.json` 与图标：定义 `display: "standalone"`、`background_color`/`theme_color: "#121417"`，提供 SVG 与 192×192、512×512 PNG 图标（确保 `vite build` 打入 `web/dist`）。
  3. `web/src/style.css`：
     - 全局声明暗黑设计 Token、`color-scheme: dark`、`accent-color: #34D399`、无衬线 UI 字体栈与等宽字体栈、`scrollbar-color`（严禁对终端内容与列表文本设 `user-select: none`）；
     - 保持 `.wterm.wterm { overflow-y: auto; }` 双类特异性，统一覆写 `.wterm.wterm` 背景 `#121417`、`border-radius: 0`、`box-shadow: none`、紧凑内边距与等宽字体栈；
     - 声明所有独立互不重叠的 `≥44×44` px 触控热区基础类（固定网格的滚动与宽度覆写留待 U4 联调定案）。
- **完成证据**：`cd web && pnpm build` 成功并在 `web/dist` 输出 `manifest.json` 与 SVG/PNG 图标。

### U3：响应式 App Shell、会话列表升级与原生 `<dialog>` 弹层

- **目标**：构建桌面双栏 / 移动单页布局，升级列表卡片信息密度，并将全部弹层替换为符合现代标准与无障碍语义的原生 `<dialog>`，同步迁移既有关闭用例。
- **涉及文件**：`web/src/App.vue`、`web/src/components/SessionList.vue`、`tests/limet-reconnect-browser-test.mjs`（S10 部分，或随 U4 一次提交前端测试）。
- **关键改动与防坑点**：
  1. 响应壳层（`<1024px` vs `≥1024px`）：移动端列表/终端全屏切换；桌面宽屏左侧会话栏 + 右侧终端工作区（保留 `:key="activeId"` 确保旧 WebSocket 干净卸载）；桌面侧栏与 U4 移动快切共享活跃会话数据源、刷新及 `authExpired` 处理。
  2. 列表展示：消费服务端投影后的 `cwd`（已折叠 `~`）与 `cmd`（仅基名），通过文本插值展示（过长路径截断保留尾部）；`closing` 集合同时禁用打开、**重命名**与关闭按钮。
  3. 原生 `<dialog>`：新建/重命名与关闭确认均使用 `showModal()`；
     - 单一 `<form @submit.prevent>` 提交入口；用 `@keydown.enter` 拦截 `event.isComposing || event.keyCode === 229` 并 `preventDefault()`；
     - 不加冗余 `role="dialog"` / `aria-modal` 或手写焦点陷阱 / Esc；关闭时清空 `returnValue = ''`；使用 `aria-labelledby`；
     - `dialog:modal` 与 `dialog::backdrop` 配 `overscroll-behavior: contain`（backdrop 加 `overflow: hidden`）；
     - `'closedBy' in HTMLDialogElement.prototype` 特性检测，仅在缺失时挂载框外 `pointerdown`+`pointerup`+`pointercancel` 复位回退；关闭确认默认 `autofocus` 落在「取消」。
  4. 同步调整 `tests/limet-reconnect-browser-test.mjs` S10：去掉旧 `page.once('dialog', ...)` 对 `confirm()` 的等待，改为点击原生确认按钮并断言打开、重命名、关闭三按钮均被禁用。
- **完成证据**：`vue-tsc -b` 零类型错误，新建/重命名/关闭（含取消、Esc、外部点击、IME 选词与并发禁用）全部正确。

### U4：终端页全面升级（快切、字号、锁定原宽固定网格、可横滑按键栏与 IME 输入条）

- **目标**：实现 Spec 007 第四、五节全部终端页交互、固定网格 CSS/滚动与跨端锁宽契约，并更新对应浏览器回归断言。
- **涉及文件**：`web/src/components/TerminalView.vue`、`web/src/style.css`（固定网格样式）、`tests/limet-reconnect-browser-test.mjs`（S9 部分）。
- **关键改动与防坑点**：
  1. **旧 worker 降级与双方向重挂载切换**：以当前连接是否收到有效 `ready.cols/rows` 决定 `supportsLockWidth`（缺失则保持自适应并禁用锁宽开关，重连重新判定）；自适应 ↔ 锁定任意方向切换均持久化后调 `connect()` 干净重挂载；锁定模式下对 `onopen`（主动发 `cols:0,rows:0`）、`onReady`、`@resize`、重连四条路径严密门禁，绝不发送有效尺寸；`pending` 队列在 `@ready` 前有序缓存二进制输出与 `size` 控制帧，确保尺寸先于对应字节应用。
  2. **字号调节与内置探针**：`10–18px` 存入 `localStorage`（首次 `<768px` `11px`、`≥768px` `13px`）；仅更新 `--term-font-size` 与 `--term-row-height` 并依赖 `@wterm/dom` 内置 `.term-size-probe` `ResizeObserver` 自动重测（**禁止**手调 `WTerm.fit()` 与软键盘适配冲突）。
  3. **固定网格 CSS 与滚动联动**：在锁定模式下覆写 `@wterm/vue` 行内 `rows * 17 + 24` 与 `WTerm._lockHeight()` 的高度，让 `.wterm` 占满容器并自身 `overflow: auto` 承接纵向滚动与 scrollback 虚拟化；为 `.term-grid` 提供显式完整宽度（`width: max-content` / `min-width: calc(var(--term-cols) * var(--term-cell-width, 1ch))`）突破 `contain: paint` 横向裁切；保证开启鼠标跟踪的 TUI 下仍有独立可操作的原生横向滚动路径。
  4. **可横滑按键栏 + 固定输入条开关**：沿用横向可滑单行按键条（`overflow-x-auto`、`overscroll-behavior-x: contain`，不引发页面横向溢出），每个按键独立 `≥44×44` px（保留 `Enter`，粘性 `Ctrl` 用 `aria-pressed`），尾端固定 `≥44×44` px「输入条」开关。
  5. **IME 输入条与焦点/视口保护**：`<form>` + `<textarea>`（`field-sizing: content` 限高、IME 安全 `Enter` 与 `Shift+Enter`、「发送」与「仅发字」）；`sendInput()` 返回 `boolean`，仅返回 `true` 时清空草稿；自动重连 `ready` 不从输入条或 `<dialog>` 抢焦点；补充轻量 `visualViewport` 壳层高度回退。
  6. **更新浏览器回归 S9**：`tests/limet-reconnect-browser-test.mjs` S9 改用稳定的 `aria-pressed` 与状态选择器替代旧 `bg-emerald-700`。
- **完成证据**：`cd web && pnpm build`（含 `vue-tsc -b`）通过；锁定模式下真浏览器验证固定网格横纵滚动、长按选中、光标聚焦与鼠标准则 TUI 均正常。

### U5：内嵌编译、端到端回归、Playwright 双端核验与收尾清理

- **目标**：端到端验证 Go `-race`、真实内嵌资源托管、顺序化 JS/Playwright 回归及锁宽核心链路，清理临时文件。
- **涉及文件**：`tests/limet-multi-view-test.mjs`、`tests/limet-reconnect-browser-test.mjs`（确认全部通过）。
- **关键改动与验收执行**：
  1. 修正 `tests/limet-multi-view-test.mjs` 第 41–48 行：改用唯一测试名并只清理本轮创建的会话 UUID，不再关闭既有「多视图」会话。
  2. 编译与资源真响应验证：
     - 后端：`PKG_CONFIG_PATH="$HOME/.local/opt/limpet-toolchain/install-1.0.1/usr/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}" go test -race -count=1 ./server`；
     - 前端与内嵌：`cd web && pnpm build`，随后 `PKG_CONFIG_PATH=... go build -tags limpet_embed -o .runtime/limpet-server-test ./server`；
     - 用内嵌测试二进制起临时开发服（`LIMPET_HOME=~/.limpet-dev`）实测 `curl -i` 校验 `/manifest.json`（`Content-Type: application/json` 且 JSON 完整）与 SVG/PNG 图标（实际图像类型与魔数/SVG 内容，绝非 `pages.go` SPA `200 text/html` 回退）。
  3. **顺序跑通既有回归脚本**（必须带 `LIMPET_HOME=~/.limpet-dev` 且显式指定 `SERVER_BIN=.runtime/limpet-server-test`，普通协议/多视图脚本与自带 server 的浏览器脚本错开顺序跑，避免端口与 `server.lock` 争用）：
     - `tests/limet-proto-test.mjs`、`tests/limet-multi-view-test.mjs`、`tests/limet-reconnect-browser-test.mjs`。
  4. **Playwright 双端专项实测与截屏**：覆盖 360px / 375px 与 1280px 视口——无宋体回退、无缝 `#121417` 画布、桌面双栏与移动快切、`<dialog>` 流程、360px 横滑按键栏与零页面横向溢出、IME 输入条发送与断线保草稿、字号调节与持久化、锁定原宽下手机持续输入（及切锁后）不抢 PC 尺寸、旧 worker 降级（如模拟无 `ready` 尺寸）及固定网格横纵滚动与 mouse-tracking TUI 操作；明确记录 Android 主屏安装、iOS 独立窗口与物理中文九宫格/语音听写需随后真机手测的覆盖边界。
  5. **收尾清理**：只清理本轮测试创建的会话 UUID，保留 `~/.limpet-dev` 原有开发会话；删除 `.runtime/` 内临时二进制与截图，核对 `git status --short` 干净且无遗留测试进程。

---

## GPT 执行与汇报要求

1. 按 `U1 → U2 → U3 → U4 → U5` 顺序实施，遵循 `AGENTS.md` 每完成一个可提交单元自动 commit（Conventional Commits + `Co-authored-by` 尾注按实际执行的 agent / 模型填写，拿不准先向用户确认，**不 push**、**绝不执行 `deploy/install.sh`** 或触碰 `~/.limpet`）。
2. 汇报遵循 `gpt-worker-handoff.md`：先给结论；异常/跳过/警告计数带总量分母，区分既有边界（如 Spec 004 多 viewer 查询回包、真机 IME/PWA 安装边界）与新问题；遇到契约分叉或真实卡点停下汇报，不擅自缩减范围；单次回归仅补跑受影响最小集，不机械重跑无关耗时项；全部完成后主动提醒用户开启新的 Codex 会话。
