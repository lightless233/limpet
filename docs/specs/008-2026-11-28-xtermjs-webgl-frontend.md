# Spec 008：前端终端引擎迁移至 xterm.js v6 + WebGL

- 日期：2026-11-28
- 状态：已定稿（完成 GPT 预审修订，可执行）
- 来源：2026-11-28 Windows RDP Chrome 真机实测复盘——在 `@wterm/dom@0.6.0` 下，长文中文打字出现明显卡顿及 Chrome 短暂白屏、高吞吐 TUI 动画受外围限帧与渲染上限影响明显掉帧、持续输出期间鼠标拖选卡顿且高亮被不透明 GPU canvas 遮挡/锚点漂移、上下半屏中文字体分裂（scrollback DOM 回退宋体 vs WebGL 视口黑体）；经 `.runtime/xterm-poc/` 隔离对接开发服务端真机验证，确认 `@xterm/xterm@6.0.0` + `@xterm/addon-webgl@0.19.0` 在流畅度、长文打字、输出中拖选与字体一致性上显著改善。
- 关联：延续并更新 [Spec 005](005-2026-11-23-heartbeat-auto-reconnect-and-systemd.md) 与 [Spec 007](007-2026-11-26-web-ui-pwa-and-lock-width.md) 的终端页前端契约；**完全保持后端 `server/`（`libghostty` 双网格、录制、重连快照、`16ms` 输出微批与 WebSocket 帧协议）零改动**。

## 目标

- **根治 Windows RDP Chrome 与高吞吐 TUI 下的卡顿、掉帧与白屏**：用 `@xterm/xterm` v6 + `@xterm/addon-webgl` 替换 `@wterm/*`（`@wterm/dom`、`@wterm/ghostty`、`@wterm/vue`），恢复原生 60fps 增量渲染，移除此前围绕 `@wterm/dom` 缺陷搭建的前端节流与捕获阶段补偿机制。
- **保障输出期间的拖选、复制与字体一致性**：利用 WebGL 原生选区渲染消除持续输出时拖选卡顿、高亮被遮与锚点漂移；显式配置中文字体回退栈，确保全屏与 scrollback 历史字体统一、无宋体回退。
- **完整保留既有跨端与移动端产品契约**：保留 Spec 005 自动重连状态机与 Spec 007 自适应/锁定原宽模式、字号调节、横滑快捷键栏及原生 IME 输入条。

## 一、前端终端引擎与渲染契约

### 1.1 运行时依赖替换

- 从 `web/package.json` 移除 `@wterm/dom`、`@wterm/ghostty`、`@wterm/vue`，并移除不再需要的类型声明 `web/src/env.d.ts`。
- 引入 `@xterm/xterm@^6.0.0`、`@xterm/addon-webgl@^0.19.0`、`@xterm/addon-fit@^0.11.0`、`@xterm/addon-unicode11@^0.9.0`、`@xterm/addon-web-links@^0.12.0`（保留既有 Vue 3 与 Tailwind CSS v4 栈）。

### 1.2 硬件 WebGL2 探测、挂载保护与上下文丢失回退

- 保留浏览器会话级一次性硬件加速探测：校验 `OffscreenCanvas`（含 `transferToImageBitmap`）与 `webgl2` 上下文（启用 `failIfMajorPerformanceCaveat: true`），结合软件渲染器标识黑名单（`swiftshader`、`llvmpipe`、`softpipe`、`software`、`microsoft basic render`）剔除软件 WebGL 实现，并在探测结束立即调用 `WEBGL_lose_context` 释放探测上下文，任何异常静默回退至 DOM 渲染器。
- **WebGL 挂载异常兜底**：由于 `OffscreenCanvas` 探测通过后 `WebglAddon` 仍需在目标 HTML `<canvas>` 上重新申请 `webgl2` 上下文，在 `term.open()` 之后加载 `WebglAddon` 必须包裹 `try/catch`——若挂载抛出异常，立即释放失败的 addon 实例，继续保留同一 `Terminal` 实例的内置 DOM 渲染器，绝不崩溃或中断会话。
- **运行期上下文丢失回退（遵循上游恢复窗口）**：订阅 `WebglAddon.onContextLoss`。按 `xterm.js` 上游 `WebglRenderer` 行为，浏览器触发 `webglcontextlost` 后会阻止默认行为并等待约 `3000ms` 的恢复窗口（`webglcontextrestored`）；若未在恢复窗口内恢复，`onContextLoss` 触发时立即 `dispose()` 并卸载 `WebglAddon`，自动切换为同一 `Terminal` 的内置 DOM 渲染器继续读写，绝不崩溃或断线。
- **软件渲染/不可用降级**：探测未通过时（软件渲染、隐私受限或无 WebGL2）不加载 `WebglAddon`，直接使用内置 DOM 渲染器。

### 1.3 拆除 `@wterm/dom` 专属补偿机制

- 全面移除原 `TerminalView.vue` 中为绕开 `@wterm/dom` 渲染瓶颈与生命周期缺陷引入的前端补偿：
  - 移除前端 `setRenderingPaused` 节流状态机、突发/慢帧计时器及 `interactiveUntil` 宽限逻辑；
  - 移除 `.terminal-viewport` 上的捕获阶段解除暂停监听；
  - 移除 `pendingCore`、`releaseCores()` 等 WASM 核心显式内存管理逻辑。
- 保留后端 `serveViewer` 的 `16ms` 爆发微批合帧，前端恢复以原生 rAF 节奏绘制。

### 1.4 字符宽度、链接、配色与字体基线

- **宽字符与链接**：启用 `Unicode11Addon`（`allowProposedApi: true` 并设 `term.unicode.activeVersion = '11'`），使代表性中文、全角符号与常见 Unicode 11 宽字符在客户端与主流 TUI 及后端 `libghostty` 的宽度基线对齐（注：部分复杂 emoji 字素簇组合在 `Unicode11Addon` 与 `libghostty` 间仍可能存在既有差异）；启用 `WebLinksAddon` 支持终端内 HTTP/HTTPS 链接可点击打开；设置 `scrollback: 10000`，保留顶部工具栏「滚到底部」功能。
- **暗黑视觉契约**（延续 Spec 007 第 1.1 节）：
  - 终端画布背景与页面无缝融合为 `#121417`，前景色 `#E6EAF0`，光标 `#34D399`（或保持清晰高对比光标），选区背景具备清晰可见的高对比半透明高亮；同步配置协调的暗黑 16 色 ANSI 调色板，不显示多余圆角/阴影/外框。
  - 终端内容区严禁设置 `user-select: none`。
- **中文字体与自适应父容器约束**：
  - 更新 `--limpet-mono` 及 `xterm` 的 `fontFamily` 配置，在通用 `monospace` 之前显式包含无衬线中文回退（`"JetBrains Mono", "Cascadia Mono", ui-monospace, "SFMono-Regular", Menlo, Consolas, "Microsoft YaHei", "PingFang SC", "HarmonyOS Sans SC", "MiSans", "Noto Sans Mono CJK SC", monospace`），彻底消除 Windows Chrome 下宋体回退与当前视口/scrollback 字体不一致问题。
  - `xterm.js` v6 通过内置 `.xterm-scrollable-element`（`SmoothScrollableElement`）自定义覆盖层滚动条管理纵向 scrollback，且 `FitAddon` 已固定预扣滚动条宽度；自适应模式下必须从外层布局严格约束 `FitAddon` 所测量的宿主容器尺寸（绝不允许内部 `.xterm-screen` 内容或滚动条反向撑开/收缩父容器宽度），从而彻底杜绝 `ResizeObserver` 测量振荡。

## 二、生命周期、重连与异步写入/改尺保序契约

### 2.1 实例生命周期与资源释放

- 每次建立新会话视图或重建连接（首连、自动重连、手动重试、切换锁定原宽）以及组件卸载时，必须：
  1. 递增连接代数并立即使旧连接的交付队列与写入/改尺屏障失效；
  2. 取消已排定的本地 rAF 调度、断开 `ResizeObserver`、先摘回调再关闭旧 WebSocket；
  3. 完整调用旧 `Terminal` 实例及所挂载全部 addon 的 `dispose()`，清空挂载容器后再重建新实例，绝不泄漏 WebGL 上下文或残留监听器。
- **正常结束（close code `1000`）排空保护**：收到 `1000` 正常退场时，不得销毁 `Terminal` 或丢弃已到达交付队列的尾部输出，确保已收到的尾帧正常解析并保留在终态画面上。

### 2.2 重连状态机与焦点/Access 语义（延续 Spec 005）

- 完整保留 `status` 状态机（`connecting` / `reconnecting` / `connected` / `ended` / `authExpired` / `error`），并保持元素 `data-status` 属性及对应状态文案、重试/刷新操作不变。
- 保留 20s `ping`/`pong` 应答、60s 活性检测（`checkLiveness`）、`online` / `visibilitychange` 立即重连、指数退避重连，以及 `/api/term` `redirect: 'manual'` 识别 Cloudflare Access 失效（`opaqueredirect` → `authExpired`）。
- 连接尚未就绪（`connecting` / `reconnecting`）期间通过 `inert` 与发送闸门拦截终端输入；重连收到 `ready` 时，若当前焦点在输入条文本框（`composerInput`）或已打开的 `<dialog open>` 内，**绝不**将焦点抢回终端。

### 2.3 写入与改尺统一因果序契约

- `xterm.js` 的 `term.write()` 经 `WriteBuffer` 异步缓冲解析（且在特定输入等路径下可能**同步**触发回调），而 `term.resize()` 同步改变网格维度，`dispose()` 也不会自动撤销已入队的写回调。为避免重连快照、实时流、远端 `size` 帧与自适应本地改尺之间发生越序或跨代污染：
  - 每轮连接（对应唯一连接代数 `gen`）独占自己的待交付事件队列与写入/改尺屏障状态。
  - **远端 `size` 帧与自适应本地改尺（`FitAddon.fit()` / 字号及软键盘引发的重算）共用同一串行屏障**：当队列前方存在待解析或解析中的输出字节时，新的 `resize` 必须排在该节点之后等待前序输出完成解析才执行；发起屏障时须先标记屏障进行中再调用 `term.write('', callback)`（兼容同步回调分支）。
  - 所有交付与写回调进入时均必须核对**连接代数仍为当前代且目标 `Terminal` 实例尚未销毁**，一旦不一致立即忽略，彻底隔离旧连接残余回调。
  - 队列顺序严格保证：**锁定原宽模式下**先应用 `ready` 携带的初始尺寸再重放快照字节；**自适应模式下**挂载并执行初次 `fit()` 后显式主动向后端发送一次初始 `{"type":"resize","cols":term.cols,"rows":term.rows}`（因为实测网格恰好为默认 `80×24` 时 `fit()` 不会触发 `term.onResize`）。

## 三、自适应模式与锁定原宽模式契约（延续并更新 Spec 007）

### 3.1 旧 worker 降级、模式切换与终端查询边界

- 仍以当前连接 `ready` 帧是否携带合法整数尺寸（`cols ∈ [2, 500]`、`rows ∈ [2, 200]`）作为 `supportsLockWidth` 的权威依据（每次重连重新判定）；旧版 worker 下维持自适应并禁用顶栏「锁定原宽」按钮且给出明确提示。
- 「自适应 ↔ 锁定原宽」双方向切换继续持久化偏好到 `localStorage` 并触发一次完整重连重建实例。
- **锁定原宽模式尺寸门禁**：`onopen` 时立即发送 `{"type":"resize","cols":0,"rows":0}`（严格先于任何 `ready` 后输入）；此后在连接周期内，无论初始就绪、容器尺寸变化、字体缩放还是重连，**严禁**向后端发送任何有效 `(cols, rows)` 申报，也绝不因本端按键或自动查询回包抢占 `s.owner` 或改变远端 PTY 尺寸。
- **自动查询覆盖边界**：`@xterm/xterm@6.0.0` 默认应答 DA/DSR/DECRPM/DECRQSS/颜色/焦点等标准查询（后端 `isTerminalReportOnly()` 已完整覆盖），但**不支持** Kitty 键盘协议（`CSI ? u`），`xterm 6.0.0` 不响应或启用该协议，具体降级表现由终端应用自行决定，主用 CLI（如 `agy` / `codex`）的实际交互表现归入 §6.3 真机验收；多 viewer 并发接入下的重复查询应答仍沿用 Spec 004 已知边界。

### 3.2 自适应模式（替代 Spec 007 第 1.2 节的 `.term-size-probe` 约定）

- 自适应模式下，不再依赖原 `@wterm/dom` 内部 `.term-size-probe`；改由 `@xterm/addon-fit`（`FitAddon.fit()`）结合容器 `ResizeObserver`（经 rAF 合并避免同帧抖动，并在连接卸载/重建时清理）以及 `window.visualViewport` 高度变化监听来重算并适配终端行列。
- 字号调节（`10px–18px`、步长 `1px`、重置默认、`localStorage` 记忆；未设置时 `<768px` 默认 `11px`、`≥768px` 默认 `13px`）与软件键盘弹出/收起改变可用视口高度时，同步更新终端字号/容器高度并经统一改尺屏障调度 `FitAddon.fit()` 上报新 `cols×rows`。字号变更优先使用 `term.options.fontSize` 原生度量与图集更新路径。

### 3.3 锁定原宽模式渲染、超高可达性与触摸滚动

- 锁定原宽模式下禁用 `FitAddon.fit()`，严格按后端下发的 `remoteSize`（`ready` 或实时 `size` 帧）固定终端行列（`term.resize(cols, rows)`）；调节字号仅更新前端字体渲染尺寸并重新同步内部画布宽高与可滚动区域，**绝不**向后端发送 `resize`。
- **双轴可滚动容器与垂直可达性**：
  - 锁宽外部容器宽度由页面可用宽度约束，内层宿主尺寸由 `.xterm-screen` 实测宽度（叠加 xterm 纵向滚动条留位）撑开以完整展示超出视口宽度的远端列；同时外部滚动容器必须支持双轴滚动（`overflow: auto; overscroll-behavior: contain`）而非禁用纵滚——当锁定的 `remoteRows` 因字号放大或软键盘弹出超过可见高度时，用户仍可在终端区域内纵滑触及最底端有效行；「滚到底部」同步将 `term.scrollToBottom()` 与外部容器滚动到底部。
  - 内部 scrollback 纵向历史滚动由 `xterm.js` v6 的 `.xterm-scrollable-element` 承接。
  - 桌面细指针下保留底部专用横向滚动条 `rail`，置于 `xterm` 鼠标事件监听树之外，按容器实际可滚范围（`scrollWidth - clientWidth`）与外层横向容器保持 `scrollLeft` 双向同步（监听宽度变化，忽略历史行增长引发的纯高度变化）；触控设备下继续隐藏 `rail`，允许原生横滑查看宽列。
  - 当内部运行的 TUI 开启鼠标跟踪并接管滚轮时，位于事件树外的 `rail` 与外层横向拖动仍保留独立可操作的横向滚动路径。
- **xterm.js 6.0.0 触摸纵滚回归修复（上游 `#5489`）**：
  - `@xterm/xterm@6.0.0` 存在单指触摸纵拖无法滚动 `.xterm-scrollable-element` 历史区（上游 `#5489`，在 6.0.0 之后才由 `#5563` 修复）的已知回归。
  - 在保留稳定包 `6.0.0` 的前提下，前端必须在终端宿主层提供范围受控的单指垂直触摸滚动处理：当触摸手势以垂直滑动为主、且当前未处于需要消费触摸的终端鼠标跟踪模式时，将垂直触摸位移换算为 `term.scrollLines()` 滚动历史（而在锁定原宽模式下继续保留原生横向触摸滑动与超视口高度网格的外层纵滑，避免两者互相抢占）。

## 四、输入、IME、快捷键与移动端交互契约

- **顶栏、快切、工具面板、按键栏与输入条**：完整保留 Spec 007 第四节定义的全部外围 UI、控件可访问标签（`aria-label` / `title` / `aria-pressed`）、`≥44×44` px 粗指针触控热区、可横滑移动按键栏（含粘性 `Ctrl` 与尾端固定「输入条」开关）、360px 零页面横向溢出，以及原生 `<form>` + `<textarea>` 输入条（「发送」带 `\r`、「仅发字」不带 `\r`、`sendInput()` 返回 `boolean` 失败保草稿、IME 组合安全拦截 `Enter`、`Shift+Enter` 换行）。
- **自动查询回包与 `ctrlSticky` 隔离**：`term.onData(chunk)` 收到终端自动产生的查询/状态回包（如以 `\x1b` 开头的序列）时，沿既有二进制 WebSocket 路径原样直发，**绝不**应用移动端 `ctrlSticky` 转换，也**绝不**消耗或重置用户已点亮的 `ctrlSticky` 状态。
- **`onBinary` 原始字节透传**：当 `xterm.js` 在 X10 鼠标报告等场景触发 `onBinary(str)` 时，必须按 Latin-1（逐字符 `charCodeAt(i) & 0xff`）转为 `Uint8Array` 发送，**禁止**使用 UTF-8 `TextEncoder` 编码破坏字节值 `≥ 0x80` 的原始鼠标坐标字节。
- **桌面剪贴板快捷键与归属契约**：
  - **复制归属**：当存在非空选区（`term.hasSelection()`）且收到 `Ctrl+C`、`Cmd+C` 或（浏览器将按键事件交由页面处理而非优先触发 DevTools 时的）`Ctrl+Shift+C` 时，在 `attachCustomKeyEventHandler` 中调用 `preventDefault()`、通过剪贴板 API 写入 `term.getSelection()` 并返回 `false`（绝不向终端发送 `\x03`）；无选区时 `Ctrl+C` 放行由 xterm 按标准终端语义发送 `\x03`。
  - **粘贴防重发归属**：普通 `Ctrl+V` / `Cmd+V` 与右键菜单粘贴由 `xterm.js` 内置的浏览器 `paste` 事件处理器**独占**完成（保留 bracketed paste）；若在自定义按键处理器中放行原生粘贴快捷键，返回 `false` 仅用于阻止 xterm 将键位再编码为控制字符，**严禁**在 `keydown` 再额外手动调用 `term.paste()` 造成双份粘贴；若额外拦截 `Ctrl+Shift+V` 且接管默认动作，须 `preventDefault()` 后仅调用一次 `term.paste()`。
- **输出期间鼠标拖选**：WebGL 模式下选区由 WebGL 层直接渲染，持续输出过程中拖动鼠标选择文字必须保持高亮不退回重排 DOM 节点、选区文本与起止锚点稳定。

## 五、非目标

以下内容明确不在 Spec 008 范围内：

- 后端 Go 代码（`server/`）及 worker/WebSocket 二进制与控制帧协议修改（包括不改动 `16ms` 微批或录制格式）。
- 会话列表与主壳层组件（`App.vue`、`SessionList.vue`、`AppIcon.vue`、`sessions.ts`）逻辑重构，或 PWA/Service Worker 变更。
- 新增前端终端搜索面板、多套浅色/自定义主题切换、Kitty 键盘/图像协议扩展实现，或升级至非稳定版 `xterm.js`（继续锁定 `6.0.0`）。
- 在 `.runtime/` 外持久化开发实验产物，或未获用户明确指令执行 `deploy/install.sh` 上线 / `git push`。

## 六、验收标准

### 6.1 构建与后端零变动基线

1. **后端零侵入与基准复用**：`git diff 69d7d59 -- server/ go.mod go.sum` 为空且工作区无未提交的后端变更；沿用已在 `69d7d59` 通过的 `go test -race -count=1 ./server` 验证基准（仅当改动触及 `server/` 时才需重新运行）。
2. **依赖与构建通过**：`web/package.json` 不再包含 `@wterm/*`；`cd web && pnpm build`（含 `vue-tsc -b` 与 `vite build`）通过；`PKG_CONFIG_PATH="$HOME/.local/opt/limpet-toolchain/install-1.0.1/usr/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}" go build -tags limpet_embed` 内嵌编译成功且可正常服务。

### 6.2 自动化浏览器回归与专项验收（`LIMPET_HOME=~/.limpet-dev`）

3. **既有端到端重连与移动交互回归**：使用显式指向新内嵌二进制的 `SERVER_BIN` 跑通 `tests/limet-reconnect-browser-test.mjs` **S1–S10 全部场景及既有断言**，并在已有移动专项中补全/核对输入条重连不抢焦点、IME 组合 `Enter` 拦截、发送失败保草稿以及「发送」（带 `\r`）与「仅发字」（无 `\r`）实际字节断言（若原断言依赖旧内部选择器仅可按等效可访问语义同步调整，不得弱化断言）。
4. **Spec 008 专项 Playwright 实测覆盖**：
   - **WebGL 探测、挂载兜底与丢失回退**：
     - 默认无头 SwiftShader 下验证生产探测准确拒绝软件渲染器并回退到 DOM 且终端可用；
     - 在测试侧通过 `page.addInitScript` 显式放行探测（不引入生产后门）挂载真实 `WebglAddon`，分别验证：触发 `webglcontextlost` 并在上游恢复窗口后自动卸载 `WebglAddon` 回退 DOM 且继续正常读写；以及挂载期抛出上下文创建失败异常时捕获并安全留在 DOM。
   - **防经典滚动条尺寸振荡**：在禁用 `--hide-scrollbars` 且终端已有 scrollback 溢出时，静止及持续输出期间无 `ResizeObserver` / `resize` 来回振荡。
   - **中文字体与 60fps 负载输出拖选**：检查计算 `fontFamily` 含中文无衬线回退；在每秒 60 帧中文彩色输出负载持续下鼠标拖拽选中终端文本，选区文本准确、无报错、过程不发生尺寸抖动。
   - **确定性时序保序与降级（可控 WS 路由）**：使用确定性 Playwright WebSocket 路由区分验证两种模式（均验证 A、B 各自按对应尺寸排版、屏幕与历史标记恰好出现一次、旧连接屏障未完成时重连回调被忽略、以及无尺寸 `ready` 触发旧 worker 降级与禁用开关）：
     - **锁宽模式**：`ready → 输出 A → 远端 size → 输出 B`（行宽/光标依赖各自远端尺寸，锁宽全程不发有效 `resize`）；
     - **自适应模式**：`ready → 输出 A → 容器尺寸变化触发本地 fit() → 输出 B`（验证本地改尺不越过前序待解析字节、自适应显式首报 `cols×rows`，且收到远端 `size` 帧**不**改变本地网格）。
   - **锁宽跨端纵横可达性、鼠标跟踪与查询隔离**：在锁定原宽模式下验证 `160` 列 × 超视口行数（如小视口/大字号或模拟键盘高度）在窄屏（如 `375px`）下整页无横向溢出，最右列与最底行均可通过滚到底部/外层横纵滚动触及，鼠标跟踪 TUI 横移后点击落点列坐标准确，调节字号不改远端尺寸；通过合成查询与输入验证锁宽按键/自动查询回包均不改 PTY、自适应自动回包不抢 `s.owner`、且 `\x1b` 查询回包不消耗移动端 `ctrlSticky`。
   - **触摸纵滚回归验证**：在 5,000+ 行 scrollback 的终端上通过 Playwright 单指垂直触摸拖动验证历史视口位置确实向回滚动。
   - **剪贴板语义闭环**：结合实际剪贴板内容与 PTY 实收字节验证：有选区时 `Ctrl+C` 复制选区且 PTY 未收到 `\x03`；无选区时 `Ctrl+C` 发送 `\x03`；普通粘贴与 bracketed paste 各仅送达一份。

### 6.3 真机手工验收边界（自动化通过后交由用户确认）

5. 自动化环境仅验证通路与语义，以下主机/驱动相关性体验由用户在真机最终拍板（不在结论中把无头 SwiftShader 或桌面模拟替代为真机硬件实测）：
   - Windows RDP Chrome（Skia Graphite + 硬件 GPU）下高吞吐动画实时流畅度、长文中文输入法连打跟手度与防白屏、输出期选区高亮与字体观感，以及主用 TUI（如 `agy` / `codex`）在无 Kitty 键盘扩展下的实际交互表现；
   - 手机端真实单指/惯性横纵滚动手感、虚拟键盘高度协同及物理中文九宫格/语音输入。
