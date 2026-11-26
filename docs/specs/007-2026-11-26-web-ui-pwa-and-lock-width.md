# Spec 007：Web 视觉重构、PWA 全屏与锁定原宽模式

- 日期：2026-11-26
- 状态：已定稿（完成 GPT 预审修订，可执行）
- 来源：2026-11-26 手机真机截图复盘与会话讨论——移动端页面字体回退为宋体、黑底套灰框画布割裂、默认字号过小列宽仅 ~40 列导致 TUI 折行、底部按键栏过宽与输入条缺失、九宫格/语音输入缺乏稳妥发送通道，且手机输入会打乱电脑端宽屏布局
- 关联：延续 Spec 003/004/005/006；**不新增 Go 与前端运行时依赖**（继续仅使用 Vue 3 + Tailwind CSS v4 + `@wterm/vue` + `@wterm/ghostty`）

## 目标

- **消除毛坯感，建立极简暗黑工具视觉语言**：彻底解决移动端宋体回退与画布边界割裂，统一全站暗黑炭灰配色、无衬线/等宽字体栈与细线图标。
- **把手机屏幕视野与单手操控做到顺手**：支持 PWA 添加主屏全屏运行（去掉浏览器上下栏）、按端自适应默认字号且可调（10–18px）、沿用单行可横滑移动按键栏并固定输入条入口、提供支持中文 IME 与语音听写的原生文本输入条、支持终端内快速切换其他活跃会话。
- **实现「锁定原宽（不抢 PC 宽度）」模式**：手机端可一键切换锁定远端 PTY 尺寸——查看和发送任何输入（及自动查询回包）均不抢占尺寸所有权或改写电脑端宽屏尺寸，同时手机端按远端真实 `cols×rows` 渲染完整 TUI（可横纵平滑滚动查看），消除互相抢宽与框线折断。
- **桌面宽屏与会话列表信息升级**：`≥1024px` 升级为双栏工作区；会话卡片展示可执行命令名（`cmd`）与投影折叠后的启动目录（`cwd`），并用原生 `<dialog>` 统一新建、重命名与关闭确认交互。

## 一、设计系统与视觉契约

### 1.1 配色与无缝画布

- **统一无缝暗黑基色**：
  - 页面底色与终端画布底色统一为 `#121417`（覆写 `.wterm` 默认 `#1e1e1e` 背景、`8px` 圆角、`box-shadow` 与过大四周内边距；移动端终端区取消外侧水平留白，仅保留紧凑内边距 `4px 6px`）。
  - 顶栏、底栏、侧边栏与弹层表层采用 `#181B20`，按钮/输入控件底采用 `#22262E`（按压反馈 `#2D333E`），极细结构分隔线采用 `#262B34`。
  - 文字对比度：主文字 `#E6EAF0`（与底色对比度 > 13:1），次要元信息与路径 `#8B94A3`（对比度 > 5:1）。
  - 语义点缀：活跃/已连接/主操作按钮 `#34D399`（翠绿，深色文字对比）；重连中 `#FBBF24`；异常与危险操作 `#F87171`。
- **原生浏览器 UI 适配**：
  - 根样式设置 `color-scheme: dark` 与 `accent-color: #34D399`；滚动条使用 `scrollbar-color` 确保滑块与轨道对比度 ≥ 3:1；**严禁**对终端内容与列表文本使用 `user-select: none`（仅可限制非内容按钮 chrome）。

### 1.2 排版与终端字号控制

- **显式字体栈**（杜绝系统默认回退为宋体）：
  - UI 无衬线字体：`"PingFang SC", "HarmonyOS Sans SC", "MiSans", "Noto Sans CJK SC", system-ui, -apple-system, "Segoe UI", sans-serif`
  - 终端与路径等宽字体：`"JetBrains Mono", "Cascadia Mono", ui-monospace, "SFMono-Regular", Menlo, Consolas, "Noto Sans Mono CJK SC", monospace`
- **字号可调与记忆**：
  - 支持在终端工具面板调节终端字号，范围 `10px–18px`（步长 `1px`），并提供「重置默认」；用户选择持久保存于 `localStorage`。
  - 未手动设置时按首次视口给默认值：窄屏（`<768px`）默认 `11px`（390px 手机可达约 55–60 列），宽屏（`≥768px`）默认 `13px`。
  - 字号变化时同步更新终端根节点的 `--term-font-size` 与 `--term-row-height`（按 `Math.ceil(fontSize * 1.25)` 等稳定像素整数映射）。
  - **严格复用 `@wterm/dom` 内置字号探针（禁止手调 `WTerm.fit()`）**：`@wterm/dom` 内置 `.term-size-probe` 的 `ResizeObserver` 在字体或行高变化时会自动触发 `_fitAutoResize()`——在两种模式下都会重新测算 `--term-cell-width` 与 `--term-row-height`，并在自适应模式下结合 `_visibleContentHeight()`（扣除软键盘遮挡高度）重算并申报 `cols×rows`。**禁止**再手动调用 `WTerm.fit()`（`fit()` 不扣除键盘遮挡，会在键盘打开时与 `_fitAutoResize()` 交替申报两组行数）；锁定原宽模式下因 `autoResize === false`，`_fitAutoResize()` 仅更新单元格测算与重绘，**绝不**改变本地列行或向外发 `resize`。

### 1.3 图标与触控热区

- 所有操作按钮（返回、菜单、新建、重命名、关闭、锁定原宽、字号增减、滚到底部、输入条开关）统一采用同尺寸细线内联 SVG 图标；所有纯图标按钮必须带明确 `aria-label` 与 `title`。
- 触控设备（`pointer: coarse`）下，每个主交互按钮自身具备独立、互不重叠的最小 `44×44` CSS px 触控热区（**禁止**用透明伪元素在相邻按钮上重叠扩展热区）；桌面细指针下可紧凑至 ≥ `32×32` px。

## 二、PWA 与视口契约

- **`web/public/manifest.json`**（采用 `.json` 扩展名确保静态文件与 `go:embed` 服务稳定输出 `application/json`）：
  - `name` / `short_name`: `"Limpet"`
  - `display`: `"standalone"`
  - `start_url`: `"/"`
  - `background_color` / `theme_color`: `"#121417"`
  - 图标配置同时覆盖 SVG 与 PNG（192×192、512×512），兼容 Android Chrome 与 iOS「添加到主屏幕」。
- **`web/index.html`**：
  - `<meta name="viewport" content="width=device-width, initial-scale=1.0, viewport-fit=cover, interactive-widget=resizes-content" />`
  - `<meta name="theme-color" content="#121417" />`、`<meta name="apple-mobile-web-app-capable" content="yes" />`、`<meta name="apple-mobile-web-app-status-bar-style" content="black-translucent" />`、`<link rel="manifest" href="/manifest.json" />` 与 `apple-touch-icon`。
- **安全区与防溢出**：顶栏、底栏与弹层适配 `env(safe-area-inset-top/bottom/left/right)`；禁止在 `html`/`body` 设置 `overflow-x: hidden` 或 `clip` 掩盖内容溢出。
- **不使用 Service Worker**：避免离线缓存导致版本陈旧或拦截 Cloudflare Access 认证重定向。

## 三、响应式布局与会话管理契约

### 3.1 响应式壳层（App Shell）

- **移动端（`<1024px`）**：列表页与终端页全屏单页切换；在终端页顶栏点击会话名触发器（带展开箭头）弹出**快速会话切换抽屉/列表**，可直接切换其他活跃会话或返回列表，不中断后台 worker。
- **桌面宽屏（`≥1024px`）**：双栏布局——左侧固定宽度会话栏（含 `+ 新建`、活跃列表、状态指示、重命名、关闭）+ 右侧全高终端工作区（未选择会话时展示简洁空态与新建引导）。
- 桌面侧栏与移动端快切抽屉共享同一份活跃会话列表、刷新逻辑与 Cloudflare Access 失效态（`authExpired`）处理。

### 3.2 会话列表信息展示

- 在保持现有核心状态管理（`loading`、`loadError`、`closing`、`authExpired`、`refreshVersion` 竞态防丢包）基础上升级卡片信息：
  - 每项显示：连接状态指示点（绿=使用中，灰=空闲）、会话名、`cmd` 徽标（如 `codex` / `agy` / `zsh`，无则省略）、启动目录 `cwd`（过长路径省略号截断并展示结尾目录）与创建时间。
  - **字段语义与脱敏边界**：
    - `cwd` 指会话启动目录（Web 创建或旧会话缺失时允许省略）；
    - `$HOME` 折叠为 `~` 的脱敏投影在服务端 HTTP 投影层（`manager.list()` 借助 `os.UserHomeDir()`）完成，从而确保 `GET /api/term` 响应本身不泄露宿主机家目录用户名，同时不改动 CLI 控制列表与注册表落盘格式；前端直接通过文本插值展示；
    - `cmd` 严格保持仅可执行文件基名（绝不包含完整 `argv`）。
  - 会话处于关闭进行中（`closing`）时，同时禁用该项的打开、**重命名**与关闭按钮，防止无效并发提交。

### 3.3 原生 `<dialog>` 弹层契约

使用原生模态 `<dialog>`（调用 `showModal()`）统一实现「新建会话」「重命名会话」和「关闭会话确认」，彻底替换浏览器原生 `confirm()` 与自定义全屏 `div` 遮罩：

- **单一提交路径与 IME 安全**：每个表单弹层使用唯一 `<form @submit.prevent>` 提交入口（确认按钮与快捷键共用同一校验逻辑）；移除 `@keyup.enter`，改在 `@keydown.enter` 检查 `event.isComposing || event.keyCode === 229` 并在组合期间调用 `event.preventDefault()` 阻止 HTML 隐式表单提交。
- **外部点击关闭与特性检测**：设置 `closedby="any"`，同时在脚本中用 `'closedBy' in HTMLDialogElement.prototype` 做特性检测——仅当浏览器不支持原生 `closedby` 时才挂载框外点击回退（同一指针在框外 `pointerdown` 且在框外 `pointerup` 才关闭，并在 `pointercancel` 时复位状态）。
- **滚动与无障碍细节**：
  - 在 `dialog:modal` 与 `dialog::backdrop` 上设置 `overscroll-behavior: contain`（且 `dialog::backdrop` 设 `overflow: hidden`）作为渐进增强，依靠应用壳层自身的 `h-dvh` 内部滚动容器约束，不在 `html`/`body` 锁滚动。
  - **不手动添加**多余的 `role="dialog"`、`aria-modal` 或手写焦点陷阱 / `Esc` 监听；使用 `aria-labelledby` 关联标题；关闭 `<dialog>` 时必须清空 `dialog.returnValue = ''`。
  - 新建/重命名表单输入默认 `autofocus`；危险操作（关闭会话确认）初始 `autofocus` 落在安全的「取消」按钮上，「确认关闭」放在不因提示位移的安全位置且使用危险语义样式。

## 四、终端页交互与中文/语音输入条契约

### 4.1 紧凑顶栏

顶栏高度保持紧凑（~44px），包含：
- 返回列表按钮（移动端主用，桌面双栏时可返回列表页/空态）；
- 会话名 + 下拉指示图标（点击拉起快速切换活跃会话面板）；
- 紧凑连接状态徽标（沿用 `connecting` / `reconnecting` / `connected` / `ended` / `authExpired` / `error` 状态机，并附带稳定的 `data-status` 属性供状态断言与样式绑定；异常态保留「重新连接」或「刷新页面」按钮）；
- 右侧快捷操作：**锁定原宽开关**（带 `aria-pressed` 语义；若当前连接的老旧 worker 不支持锁宽契约则禁用并给出明确状态提示）与**更多工具弹出菜单**（字号 `-` / 当前字号 / `+` / 重置、滚到底部）。

### 4.2 移动端快捷操作栏（沿用左右滑动单行设计）

仅在移动端视口（或触控布局）显示，沿用现有横向滑动单行按键条，并显式固定输入条入口：
- **可横滑按键条**（`overflow-x-auto`、`overscroll-behavior-x: contain`，严格限制在按键条自身容器内滑动、**绝不**引发页面整体横向溢出）：每个按键保持独立且不重叠的 `≥44×44` CSS px 触控尺寸，保留 `Esc`、`Ctrl+C`、`Tab`、`↑`、`↓`、`←`、`→`、`Enter` 及粘性 `Ctrl`（`Ctrl` 带 `aria-pressed` 反映激活态）。
- **固定尾端输入条开关**：在底栏右侧固定独立的「输入条」开关按钮（同样满足 `≥44×44` px，与左侧可横滑按键区以细分隔线隔开），保证无论按键栏滑到哪里都能一触展开/收起中文与语音输入条。
- 各普通键发送后焦点归还终端（保持 Spec 004/005 交互习惯）；`Ctrl` 保持粘性修饰键逻辑。

### 4.3 可展开文本 / 语音输入条（IME Composer）

为彻底解决移动端中文九宫格联想与语音听写直接输入终端字符流容易丢字/错位的问题，提供可随时展开/收起的底部原生输入条：

- 展开时位于终端区与底部按键栏之间：依托 `interactive-widget=resizes-content`，同时在终端页壳层补充轻量的 `visualViewport` 高度同步回退（照顾不支持 `interactive-widget` 的 Safari / 独立 Web App 窗口），确保软键盘弹出时输入框与终端末行仍可同时操作。
- 使用原生 `<form>` + `<textarea>`（配合隐藏/无障碍 `<label>`、`field-sizing: content` 自动随内容增高并限定最大高度如 4–5 行，保持 `resize: vertical` 或配合容器控制，不用裸 `resize: none`），支持任意中文输入法、手机语音输入法、多行粘贴。
- 提供两个明确按钮动作：
  - **主提交按钮「发送」**（`<button type="submit">`）：按当前文本经既有 64KiB 分块 `sendInput` 发送文本 + `\r`；
  - **辅助按钮「仅发字」**：发送文本不带 `\r`，方便用户继续补参数或按手机按键栏的 `Tab`/`Ctrl+C`。
- **草稿防丢与焦点保护**：
  - `sendInput()` 必须返回 `boolean`（仅当 `ws.readyState === WebSocket.OPEN && status === 'connected'` 且全部数据分块成功交由 `ws.send` 时返回 `true`）；输入框仅在 `sendInput()` 返回 `true` 后才清空草稿；非连接态禁用提交并完整保留文本。
  - 自动重连收到 `ready` 时，若当前焦点正位于输入条 `<textarea>` 或已打开的原生 `<dialog>` 内，**不得**抢夺焦点回终端。
- 键盘快捷键：物理/外接键盘按 `Enter`（非 `Shift+Enter`）触发「发送」，严格拦截 `event.isComposing || event.keyCode === 229` 并 `preventDefault()` 防止确认拼音候选词时误提交；`Shift+Enter` 插入换行。

## 五、「锁定原宽（不抢 PC 宽度）」跨端行为与协议契约

### 5.1 前端模式切换、兼容降级与固定网格渲染契约

1. **旧 worker 能力检测与降级**：
   - 以当前 WS 连接收到的 `ready` 控制帧中是否包含有效正整数 `cols` 与 `rows` 作为**该连接支持锁定原宽**的权威依据（每次重连重新判定）。
   - 若服务端连着仍在运行的旧版 worker（`ready` 无尺寸字段，且旧 worker 会忽略 `0×0`、也不推送 `ftSize`），当前连接保持自适应模式并禁用顶栏「锁定原宽」开关，防止用户开启后产生“假锁宽”错觉。
2. **两种模式与统一切换生命周期**：
   - 因 `@wterm/vue` 的 `autoResize` 仅在挂载时初始化，**「自适应 ↔ 锁定原宽」双方向切换均采用统一收敛路径**：持久化偏好到 `localStorage` 后触发一次干净的 `connect()` 重建实例与重连（期间通过 `inert` 与 `status !== 'connected'` 暂停输入直至 `ready` 恢复；允许重建过渡期间画面瞬时清空，重放完成后的最终网格与原 PTY 状态一致）。
   - **自适应模式（默认）**：与 Spec 004 一致，终端以 `auto-resize` 铺满容器，接入与尺寸变化时上报本端测算出的 `cols×rows`，本端产生 `ftInput` 时触发 worker 晋升该尺寸。
   - **锁定原宽模式**：
     - **严禁发送任何有效尺寸申报**：同时约束 `onopen`、`onReady`、`@resize` 与重连逻辑，锁定模式下**绝不**向后端发送具体 `cols×rows`；在 `onopen` 时立即主动发送清空控制帧 `{"type":"resize","cols":0,"rows":0}`，此后本端发送任何输入或自动查询回包**均不抢占 `s.owner` 也不改动远端 PTY 尺寸**。
     - **保住worker当下尺寸**：切换为锁定原宽时保持并渲染 worker **当时**的 `cols×rows`（已经被自适应端改窄的会话不会自动回退恢复更早的 PC 历史宽度）。
     - **有序消费 `ready`/`size` 与 VT 字节**：在终端 `@ready` 触发前的 `pending` 队列中按到达先后统一存放二进制输出块与实时 `size` 更新（在消费后续 VT 字节前应用对应 `cols×rows`），收到实时 `{"type":"size","cols":...,"rows":...}` 时直接更新固定 `cols`/`rows`（不清空画面、不重连）。
3. **固定网格 CSS 与滚动契约（锁定原宽下）**：
   - **纵向滚动留给 `.wterm`**：锁定模式下必须同时覆写 `@wterm/vue` 的行内 `style.height = rows * 17 + 24` 与 `WTerm._lockHeight()` 写入的行内高度，让 `.wterm` 严格占满视口容器高度并自己负责 `overflow: auto`（从而完整保留 `@wterm/dom` 的 scrollback 虚拟化、贴底自动跟随与「滚到底部」功能）。
   - **显式网格宽度突破 `contain: paint` 裁切**：`@wterm/dom` 对 `.term-grid` 设置了 `contain: layout paint style`。锁定模式下必须为 `.term-grid` 赋予完整网格宽度（如 `width: max-content` / `min-width: calc(var(--term-cols) * var(--term-cell-width, 1ch))`），防止超出视口宽度的右侧列被画布裁掉而无法横滑查看。
   - **鼠标跟踪 TUI 下的横移保障**：开启鼠标跟踪的 TUI 会由 `.term-grid` 捕获内部滚轮事件并转为终端鼠标序列，因此固定网格容器必须保留独立可操作的原生横向滚动条与边缘横滑操作路径。
4. **既有已知边界与新增保证**：
   - Spec 004 记录的「多 viewer 连接下各端可能重复回复终端自动查询（如 `\x1b[6n`）」仍属既有覆盖边界；Spec 007 在此基础上保证：锁定原宽 viewer 产生的自动查询回包同样走 `v.locked` 分支，**绝不**抢占 `s.owner` 或改变 PTY 尺寸。

### 5.2 后端与协议最小增量（完全向后兼容）

1. **`GET /api/term` 元数据扩展（HTTP 投影层）**：
   - `manager.list()` 在既有 `{id, name, created, attached}` 显式白名单对象上，仅当非空时增加 `cwd`（经 `os.UserHomeDir()` 将家目录前缀折叠为 `~`）与 `cmd`（已限制为可执行文件基名）。绝不改变 CLI 控制列表、注册表或泄露完整 `argv` / 本地用户名。
2. **显式清空申报 `0×0` 与所有权保护（`inputMu` 下串行）**：
   - `bridge.go` 与 `workerapp.go` 解析 `resize` 控制帧时使用指针字段 `*uint16` 要求 `cols` 与 `rows` 键必须**显式存在且非 `null`**（防止 `{"type":"resize"}` 或 `null` 字段因 Go 零值误触发清空）。真实尺寸 `valid()` 范围（`2..500 × 2..200`）保持不变。
   - 在 `viewer` 上增加 `locked bool`（由 `s.inputMu` 保护）：
     - 收到显式 `(0, 0)`：`v.locked = true`，清空 `v.dims`，若 `s.owner == v` 则置 `s.owner = nil`；
     - 收到有效 `(cols, rows)`：`v.locked = false`，更新 `v.dims`，若 `s.owner == v && !v.closed.Load()` 则调用 `s.resize(...)`；
     - 收到 `ftInput` 时在 `s.inputMu` 下判定：若 `v.locked == true`，**仅写 `s.ptmx`（绝不执行 `s.owner = v` 也不调用 `s.resize`）**；若 `v.locked == false`，**完整保留 Spec 006 对未申报即输入 CLI 客户端的既定语义**（`if s.cli { s.owner = v }` 且 `if v.dims.valid() { s.resize(...) }`）。
3. **带实时尺寸的 `ftReady` 与保序有界 `ftSize`（`0x07`）**：
   - **快照与 `ftReady` 同锁取尺**：`rebuildFor(v)` 在持有同一把 `s.gridMu` 锁期间同时取出并对齐返回 `(out, cont, curCols, curRows, err)`（确保快照与 `ready` 声明的尺寸绝不撕裂）；首次无重放接入（`replay == false`）同样取出真实初始尺寸填入 `ftReady` 载荷 `{"cols":..., "rows":...}`。`bridge.go` 收到有效尺寸的 `ftReady` 时转发 `{"type":"ready","cols":...,"rows":...}`；若载荷为空或无效（如旧版 worker / 现有测试）仍转发 `{"type":"ready"}`。
   - **同队列严格保序的 `ftSize`（`0x07`）**：将 `viewer.queue` 从纯字节切片队列升级为同一条有界 FIFO 事件队列（`chan viewerEvent`，每个元素承载 `ftOutput` 字节块或 `ftSize` 维度快照）。当 `s.resize(cols, rows)` 在 `gridMu` 内实际变更 PTY 与双网格尺寸后，按既定锁序（`inputMu -> gridMu -> viewersMu`，并在 `gridMu` 持有下更新切点或遵循既有 `fedBytes` 顺序）非阻塞向所有未关闭且切点已跟上的存活 viewer 入队 `ftSize` 事件（队列满则仅以 `4001` 丢弃该慢 viewer，绝不阻塞 `s.resize` 或 `readLoop`）。
   - **单写者与退场排空语义不变**：`serveViewer` 仍是 `v.conn` 的唯一写者——从同一个 `v.queue` 按 FIFO 顺序写出：遇到 `ftSize` 前先把之前缓存待合批的 `ftOutput` 写出，再写 `ftSize` 帧，再继续写新尺寸下的后续 `ftOutput`；正常退出关闭 `v.queue` 后仍先排空尾帧（含尾部 `ftSize`/`ftOutput`）再发 `ftExit`。`bridge.go` 将 `ftSize` 翻译为 WS 文本帧 `{"type":"size","cols":...,"rows":...}`；本地 CLI viewer 对未知帧照常忽略。

## 六、非目标

以下内容明确不在 Spec 007 范围内：

- 已结束会话列表、历史录制回放或 `codex resume` 一键重开界面（留待 Spec 008）。
- Service Worker 离线资源缓存与推送通知。
- 多套主题切换（亮色模式、自定义颜色选择器）或随包内嵌大型字体文件。
- 锁定原宽模式下的自定义双指捏合缩放手势接管（直接通过 `A-`/`A+` 调节字号 + 原生横纵滚动查看，避免与移动端文本选中、惯性滚动冲突）。

## 七、验收标准

1. **视觉与字体基线**：移动端（360px / 375px）与桌面端（1280px）页面全部采用无衬线 UI 字体与等宽终端字体，**无宋体回退**；页面与终端画布底色统一为 `#121417`，无割裂黑边灰框；图标全站一致且纯图标按钮具备 `aria-label` 与互不重叠的 `≥44×44` px 触控热区。
2. **PWA 可安装与独立视口**：在 `limpet_embed` 构建下实际 `curl` 验证 `/manifest.json`（`Content-Type` 为 `application/json` 且 JSON 字段完整）与 SVG/PNG 图标（返回实际图标媒体类型与非 HTML 正文，未回落 SPA `index.html`）；`index.html` 配置 `viewport-fit=cover`、`interactive-widget=resizes-content` 与安全区适配。
3. **响应式布局与会话管理**：桌面端展示左侧会话栏 + 右侧终端工作区；移动端可在终端顶栏打开快速切换面板直接切换另一会话并共享刷新/Access 失效态；`GET /api/term` 校验有/无元数据两种情况下的精确白名单字段、`~` 折叠及真实启动参数标记不泄露；关闭会话期间同时禁用打开、重命名与关闭按钮。
4. **原生 `<dialog>` 交互与安全**：新建、重命名与关闭确认均通过原生模态 `<dialog>` 完成；支持 `overscroll-behavior: contain`、特性检测的外部点击关闭、关闭确认默认聚焦「取消」，以及 `<form>` + `keydown` 防中文 IME 候选确认误提交。
5. **移动端可横滑按键栏与输入条**：沿用单行横滑按键栏（`Esc`/`Ctrl+C`/`Tab`/方向键/`Enter`/粘性 `Ctrl`），配合固定在尾端的「输入条」按钮，在 360px 宽屏下**页面本身无任何横向溢出**；展开输入条后支持多行与中文/语音「发送」（带 `\r`）与「仅发字」（不带 `\r`），仅在 `sendInput()` 返回 `true` 后清空草稿，自动重连 `ready` 不从输入条或弹层抢焦点。
6. **字号调节与持久化**：在 `10px–18px` 内调节字号与重置可实时改变终端排版与行高并持久化于 `localStorage`；通过 `.term-size-probe` 自动触发尺寸重算（不手调 `fit()`），自适应模式下按扣除软键盘后的可视高度上报新 `cols×rows`，锁定模式下只改渲染不改远端尺寸。
7. **锁定原宽与旧 worker 降级契约**：
   - 连接新 worker 时开启「锁定原宽」，从手机端通过输入条/按键栏/键盘（及模拟查询回包）持续输入，PTY 保持切锁时尺寸。CLI 已是 owner 时其后继 `SIGWINCH` 立即生效。手机原为 owner 时切锁清空 owner，CLI 首次输入重新取得所有权，之后 `SIGWINCH` 立即生效；同时手机端按远端真实 `cols×rows` 完整渲染，支持真实滚轮/触摸横纵滚动、长按文本选中、光标聚焦及开启鼠标跟踪的 TUI 下仍可独立横向滚动；
   - 连接不返回 `ready.cols/rows` 的旧版 worker 时，保持自适应模式并禁用锁宽开关；
   - 自适应 ↔ 锁定重挂载切换后终态画面与远端 PTY 一致；重连期间画面瞬时清空后重放对齐属于预期行为。
8. **既有协议与并发保序验证**：`go test -race -count=1 ./server` 全部通过，覆盖：缺失/`null` 字段的 `resize` JSON 防误清、显式 `(0,0)` 后不抢 `s.owner` 与 Spec 006 CLI 未申报即输入语义保留、人为卡住重放/输出泵时交错 `resize` 与 VT 的 `ready`/`size`/输出帧严格保序、空载荷旧 `ftReady` 兼容、`TestCLIWorkerActiveSize` 更新后通过。
9. **前端构建、回归与真机交互边界**：`cd web && pnpm build`、`go build -tags limpet_embed` 全部通过；使用显式指向新二进制的 `SERVER_BIN` 与 `LIMPET_HOME=~/.limpet-dev` 顺序跑通更新后的 JS/Playwright 回归脚本（`limet-multi-view-test.mjs` 仅清理本轮 UUID），并截屏检查 360px/375px 与 1280px 核心视觉与交互链路（Android 主屏安装、iOS 独立窗口软键盘行为、物理中文九宫格与语音听写需随后在真机上完成人工交互验收）；清理临时 `.runtime/` 产物保持工作区干净。
