# Plan 008：前端终端引擎迁移至 xterm.js v6 + WebGL

- 日期：2026-11-28
- 对应 Spec：`docs/specs/008-2026-11-28-xtermjs-webgl-frontend.md`（已同步 GPT 预审修订）
- 执行方式：DELEGATED（适合直接转交 GPT / Codex 实施与验收，按 `U1 → U2 → U3` 顺序推进）
- 约束基线：
  - **后端零改动**：严禁修改 `server/`、`go.mod`、`go.sum` 任何文件；若执行 `git diff server/` 必须为空，后端单测沿用已在 `69d7d59` 通过的 `-race` 基准。
  - **环境隔离与上线红线**：所有开发 server 与浏览器回归必须设置 `LIMPET_HOME=~/.limpet-dev`；**严禁**执行 `deploy/install.sh`、**严禁**触碰 `/opt/limpet` 与正式 `~/.limpet`；不要加入仅服务于测试的生产开关后门。
  - **进程清理安全**：停掉自己起的临时测试 server 只用普通 `kill`（绝不使用按进程树整棵清理的方式，也不要影响已有其他会话或用户进程）；`pgrep -f`/`pkill -f` 若使用需遵循 `[x]yz` 形式。
  - **Git 规范**：遵循 `AGENTS.md`，每完成一个可提交单元自动 commit（英文 Conventional Commits，`Co-authored-by` 尾注按实际执行的 agent / 模型填写，拿不准先向用户确认，**不主动 push**）。

## 总体顺序与依赖关系

```text
U1（替换 @wterm/* 为 xterm.js v6 + WebGL/Fit/Unicode11/WebLinks，重写核心链路、统一因果屏障、查询/剪贴板与拆除旧节流）
  └──▶ U2（自适应父约束与首报、锁宽双轴可滚动布局 + rail 同步、触摸纵滚修复 #5489、字号与样式收敛）
         └──▶ U3（vite + limpet_embed 构建、S1–S10 + 移动补充回归、Spec 008 专项 Playwright 验收与收尾清理）
```

## 实施单元

### U1：依赖替换、核心引擎迁移与统一写入/改尺屏障

- **目标**：卸载 `@wterm/*`，接入 `@xterm/xterm` v6 及其官方 addon，落实现连接代数独占的统一写入/改尺屏障、WebGL2 硬件探测与挂载/丢失双重兜底、查询与剪贴板完整契约、Spec 005 重连/退场排空语义，并拆除旧版 `setRenderingPaused` 节流补偿。
- **涉及文件**：`web/package.json`、`web/pnpm-lock.yaml`、`web/src/env.d.ts`（删除）、`web/src/style.css`（基础 `.xterm` 与字体栈）、`web/src/components/TerminalView.vue`。
- **关键改动与防坑点**：
  1. **依赖与类型清理**：
     - 在 `web/` 移除 `@wterm/dom`、`@wterm/ghostty`、`@wterm/vue` 及 `web/src/env.d.ts`，安装 `@xterm/xterm@^6.0.0`、`@xterm/addon-webgl@^0.19.0`、`@xterm/addon-fit@^0.11.0`、`@xterm/addon-unicode11@^0.9.0`、`@xterm/addon-web-links@^0.12.0`。
  2. **引擎挂载、WebGL 挂载捕获与 `onContextLoss` 兜底**：
     - 保留现有会话级硬件 WebGL2 `detectRenderer()` 探测；创建 `Terminal`（`scrollback: 10000`、`allowProposedApi: true`、暗黑配色 `#121417`/`#E6EAF0`/高对比光标选区与 16 色 ANSI 调色板、含中文无衬线回退的 `fontFamily`、`fontSize`）。
     - 挂载 `FitAddon`、`Unicode11Addon`（`term.unicode.activeVersion = '11'`）、`WebLinksAddon`。硬件可用时在 `term.open()` 后用 `try/catch` 挂载 `WebglAddon`：创建失败立即释放失败 addon 并留在 DOM；挂载成功则监听 `onContextLoss`（遵循上游 ~3s 恢复窗口），触发时立即 `dispose()` 并卸载 `WebglAddon` 安全回退至 DOM。
  3. **连接代数生命周期与 `1000` 退场保护**：
     - 每次 `connect()` 重建、切锁宽或 `onBeforeUnmount`：先递增连接代数使旧交付队列、写屏障、rAF 与监听器全部失效，断开 `ResizeObserver`，先摘回调再关闭旧 WebSocket，再完整 `dispose()` 旧 `Terminal` 与全部 addon 并清空挂载节点。
     - 收到 `1000` 正常会话结束时不销毁或清空终端，让已在交付队列中的尾帧完整解析落地。
  4. **拆除 `@wterm/dom` 旧补偿 + 统一 write/resize 因果屏障**：
     - 彻底删除 `setRenderingPaused` 整套节流状态机、定时器/rAF、`writing` 守卫、`interactiveUntil`、捕获阶段监听及 `pendingCore`/`releaseCores()`。
     - 建立由当前连接代数独占的串行交付队列：**远端 `size` 帧与本地自适应改尺（`FitAddon.fit()`）共用同一屏障**。当队列/写入未落定期间到来新改尺或 `size` 事件时，先标记屏障进行中再调用 `term.write('', callback)`（避免 `WriteBuffer` 同步执行回调导致状态错乱），回调入口必须校验当前连接代数与目标 `Terminal` 实例一致后才执行 `term.resize()` 或 `fit()` 并推进后续队列。
     - 锁宽模式下 `ready` 初始尺寸先于 `pending` 快照写入应用；自适应模式下初次 `fit()` 完毕后显式发送一次 `{"type":"resize","cols":term.cols,"rows":term.rows}`，避免刚好匹配默认 `80×24` 时漏发首报。
  5. **输入、查询隔离与剪贴板归属**：
     - `term.onData` 与移动按键栏、输入条共用返回 `boolean` 的 `sendInput()`（失败保草稿）；以 `\x1b` 开头的自动查询/状态回包原样发送，**严禁**触发或消耗 `ctrlSticky`；`term.onBinary` 按 Latin-1（`charCodeAt(i) & 0xff`）逐字节转 `Uint8Array`。
     - `attachCustomKeyEventHandler`：当 `term.hasSelection()` 且按下 `Ctrl+C`、`Cmd+C` 或（浏览器下发至页面的）`Ctrl+Shift+C` 时，`preventDefault()`、复制选区并返回 `false`（不发 `\x03`）；无选区放行 `\x03`。普通粘贴由 xterm 内置 `paste` 事件独占处理，禁止 `keydown` 与 `paste` 双重发送；仅当额外接管 `Ctrl+Shift+V` 等不触发原生粘贴的键位时，`preventDefault()` 并调用一次 `term.paste()`。
     - 保留 Spec 005 重连状态机（`data-status`、`inert`、20s `ping`/60s `checkLiveness`、Access 检测、重连不抢 `composerInput`/`<dialog open>` 焦点），并记录主用 TUI（`agy`/`codex`）在 `6.0.0` 下无 Kitty 键盘协议时的实际回退情况以供核对。
- **完成证据**：`cd web && pnpm build`（`vue-tsc -b` + `vite build`）通过；接入本地开发服能正常挂载、输入及触发 WebGL/DOM 回退且无生命周期报错。

### U2：自适应约束、锁宽双轴滚动布局、触摸纵滚修复（`#5489`）与样式收敛

- **目标**：完善自适应与锁定原宽两种几何与滚动体系，解决 `xterm.js` v6 锁宽超高行裁切与 `6.0.0` 触摸纵滚回归，统一桌面 `rail` 与移动端触控体验。
- **涉及文件**：`web/src/components/TerminalView.vue`、`web/src/style.css`。
- **关键改动与防坑点**：
  1. **自适应父容器严格约束与 `visualViewport` 协同**：
     - 明确 `xterm.js` v6 使用 `.xterm-scrollable-element`（`SmoothScrollableElement`）管理纵向历史且 `FitAddon` 已预扣滚动条宽度；通过 CSS 严格保证 `FitAddon` 测量的父容器尺寸只由外层 Flex/视口决定，不因内部 `.xterm-screen` 或滚动条反过来撑缩父宽，杜绝经典滚动条平台上的宽度振荡。
     - 结合容器 `ResizeObserver`（rAF 合并）与 `window.visualViewport` 变化，经 U1 的统一改尺屏障调度 `FitAddon.fit()`，并按 `term.onResize` 上报最新 `cols×rows`。
  2. **锁宽门禁、双轴可达性与 `rail` 同步**：
     - `supportsLockWidth` 仍由 `ready` 是否含有效尺寸判定；锁宽模式下 `onopen` 发 `0×0`，且容器变化、字体缩放与 `visualViewport` 变化绝不触发 `FitAddon.fit()` 或向外发任何 `resize`。
     - 外层滚动容器由可用页面宽约束并设为双轴可滚（`overflow: auto; overscroll-behavior: contain`），内部根据 `.xterm-screen` 实测宽（加纵向滚动条留位）展开完整网格：这样当 `remoteRows` 在放大字号或软键盘弹出后超出可用高度时，最底端活动行仍可在终端区内纵滑可达（绝不类似 `overflow-y: hidden` 直接裁死）。「滚到底部」同步调用 `term.scrollToBottom()` 与外层容器滚到底部。
     - 桌面细指针下底部 `rail` 放在 `xterm` 事件树之外，按容器实际横向可滚距离（`scrollWidth - clientWidth`）与外层横向容器双向同步 `scrollLeft`（监听宽度变化忽略纯高度变化）；触控设备下隐藏 `rail`。
  3. **修复 `xterm.js 6.0.0` 触摸纵滚回归（`#5489`）**：
     - 在保持稳定 `6.0.0` 的前提下，为终端区补充轻量的单指触摸纵滚支持：当检测到垂直滑动为主且未处于消费触摸的鼠标准则追踪时，按行高把触摸纵向位移转为 `term.scrollLines()` 滚动 `.xterm-scrollable-element` 历史；若水平滑动为主或在锁宽超视口网格下边界溢出时，让位给外层容器原生横滑/纵滑。
  4. **字号与废弃样式清理**：
     - 清理 `web/src/style.css` 中废弃的 `.wterm.wterm`、`.term-grid` 等旧规则；落位字号 `10–18px` 调节与重置（`<768px` 首默认 `11px`、`≥768px` 首默认 `13px`，`localStorage` 持久化）：自适应经屏障 `fit()`，锁宽仅改字号与重测横向宽度。
- **完成证据**：`cd web && pnpm build` 通过；验证自适应不振荡、锁宽超出视口宽高的网格最右列与最底行皆可达且 `rail` 协同正常、单指触摸可稳定滚动 scrollback 历史。

### U3：内嵌编译、端到端回归、专项 Playwright 验收与收尾清理

- **目标**：用真实 `limpet_embed` 二进制跑通全部既有浏览器场景及 Spec 008 专项验收，确认后端零改动与仓库干净。
- **涉及文件**：仅在确需适配定位/补充移动输入条断言时更新 `tests/limet-reconnect-browser-test.mjs`（不弱化原有断言）；专项 Playwright 测试可写入临时校验脚本并在清理阶段移除或按需精简保留。
- **关键改动与验收执行**：
  1. **零改动核对与内嵌构建**：
     - 校验 `git diff server/ go.mod go.sum` 为空（后端沿用 `69d7d59` 通过的 `go test -race -count=1 ./server` 基准，不机械重跑）；
     - 执行 `cd web && pnpm build`；
     - 带 `PKG_CONFIG_PATH="$HOME/.local/opt/limpet-toolchain/install-1.0.1/usr/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"` 编译 `go build -tags limpet_embed -o .runtime/limpet-server-008 ./server`。
  2. **既有浏览器回归（`LIMPET_HOME=~/.limpet-dev`）**：
     - 运行 `LIMPET_HOME=~/.limpet-dev SERVER_BIN="$PWD/.runtime/limpet-server-008" node tests/limet-reconnect-browser-test.mjs`，确认 **S1–S10 全部场景及既有断言**通过；在已有移动测试中补足输入条重连不抢焦点、IME `Enter` 拦截、发送失败保草稿与「发送」/「仅发字」实发字节断言。
  3. **Spec 008 专项 Playwright 验收**（均带 `LIMPET_HOME=~/.limpet-dev`，禁止引入生产后门）：
     - **WebGL 探测与上下文回退**：默认 SwiftShader 验证生产探测安全拒绝并回退 DOM；测试侧用 `page.addInitScript` 放行探测加载真实 `WebglAddon`，分别实测触发 `webglcontextlost` 在恢复窗口后卸载回退 DOM 并可继续读写，以及模拟挂载抛出失败异常后安全留在 DOM。
     - **经典滚动条与 60fps 负载拖选**：`ignoreDefaultArgs: ["--hide-scrollbars"]` 下有 scrollback 溢出静止及持续输出零 `resize` 振荡；校验计算 `fontFamily` 含中文无衬线回退；60fps 中文彩色输出负载进行中鼠标拖选，确认选区准确非空、过程零尺寸抖动、零报错。
     - **确定性 WS 路由保序与降级**：用 Playwright WebSocket 路由驱动 `ready → 输出 A → size → 输出 B`（A、B 行宽/光标依赖各自尺寸），验证终态屏与历史标记恰好出现一次、自适应首报 `cols×rows`、旧连接未完成屏障在重连后被忽略、无尺寸 `ready` 正确降级禁用锁宽开关。
     - **锁宽可达性、mouse-tracking 校对、查询与触摸滚动**：验证锁宽 `160` 列 × 超高网格在 `375px` 视口下整页无横向溢出且最右列与最底行皆可达，mouse-tracking TUI 横移后点击列坐标准确，调字号不改远端尺寸；通过合成输入与查询回包验证锁宽输入/回包不改 PTY、自适应回包不抢 `s.owner`、`\x1b` 查询回包不消耗 `ctrlSticky`；验证单指触摸拖动可改变历史 `viewportY`。
     - **剪贴板语义与实例释放**：校验选区 `Ctrl+C` 复制成功且 PTY 无 `\x03`、无选区 `Ctrl+C` 发送 `\x03`、普通粘贴与 bracketed paste 各仅送达一份，以及多次会话切换/切锁宽重连后无报错或连接泄漏。
  4. **收尾清理**：
     - 关闭并仅删除本轮验收创建的临时 dev 会话 UUID，保留原有 `~/.limpet-dev` 会话；用普通 `kill` 停掉本轮临时测试 server；删除 `.runtime/limpet-server-008` 与本轮临时脚本/截图（注意保留用户原有 `.runtime/` 文件），确认工作区干净（或仅含预期提交）且 `git diff server/` 仍为空。

---

## GPT 执行与汇报要求

1. 严格按 `U1 → U2 → U3` 顺序实施；遵循 `AGENTS.md`，每完成一个可提交工作单元自动 `commit`（英文 Conventional Commits + 真实执行的 agent/模型 `Co-authored-by` 尾注，拿不准先向用户确认），**禁止主动 push**、**禁止触碰 `~/.limpet`**、**禁止执行 `deploy/install.sh`**，停自启临时 server 只用普通 `kill`。
2. 执行与最终汇报遵循 [gpt-worker-handoff.md](file:///home/lightless/.gemini/config/rules/gpt-worker-handoff.md)：
   - **先给业务结论与验证状态**，再展开明细，严禁堆砌免责声明；
   - **所有非零异常、告警、跳过或未解析计数必须带总量分母**，并明确区分既有真机/覆盖边界（如 Spec 004 多 viewer 重复应答、Kitty 键盘缺失、`6.3` 真机硬件 GPU 与触控手感边界）与本轮新增问题；
   - 遇到真实技术卡点或契约分叉时先用通俗语言说明原因、方案代价与推荐，未经同意不得擅自缩减范围；
   - 单项复测按最小缺口验证，不机械重跑无关长耗时全套（如 `server/` 未动时不重复全套 Go `-race`）；
   - 全部单元完成并收尾后，**主动提醒用户开启新的 Codex 会话**。
