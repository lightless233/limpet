# Plan 009：前端可配置回滚行数（含 0 = 近似无限）与前后端联动

- 日期：2026-11-28
- 对应 Spec：`docs/specs/009-2026-11-28-configurable-scrollback.md`（待 GPT 预审）
- 执行方式：DELEGATED（适合转交 GPT / Codex 实施与验收，按 `U1 → U2` 顺序推进）
- 约束基线：
  - **环境隔离与上线红线**：所有开发 server、Go 单测与浏览器回归必须使用 `LIMPET_HOME=~/.limpet-dev`（Go 单测用 `t.TempDir()`）；**严禁**执行 `deploy/install.sh`、**严禁**触碰 `/opt/limpet` 与正式 `~/.limpet`；不要加入仅服务于测试的生产开关后门。
  - **工具链环境变量**：编译与运行 Go 测试需带 `PKG_CONFIG_PATH="$HOME/.local/opt/limpet-toolchain/install-1.0.1/usr/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"`。
  - **进程清理安全**：停掉自己起的临时测试 server 只用普通 `kill`（绝不使用按进程树整棵清理的方式）；`pgrep -f`/`pkill -f` 若使用需遵循 `[x]yz` 形式。
  - **Git 规范**：遵循 `AGENTS.md`，每完成一个可提交单元自动 commit（英文 Conventional Commits，`Co-authored-by` 尾注按实际执行的 agent / 模型填写，拿不准先向用户确认，**不主动 push**）。

## 总体顺序与依赖关系

```text
U1（后端 settings.json 持久化、HTTP /api/settings、libghostty 字节上限修复、worker 启动读取与运行期 set_scrollback 联动）
  └──▶ U2（前端配置同步与 #terminal-tools 表单交互、limpet_embed 端到端 Playwright 验收与收尾清理）
```

## 实施单元

### U1：后端配置持久化、HTTP 接口与 worker/双网格联动

- **目标**：落地 `<LIMPET_HOME>/settings.json` 读写与损坏恢复、`GET/PUT /api/settings` 串行化接口与在线广播、worker 启动读取与 `set_scrollback` 控制握手，并修复 `libghostty` 双网格默认 `10,000` 字节上限截断问题。
- **涉及文件**：`server/main.go`、`server/handlers.go`、`server/manager.go`、`server/proto.go`、`server/bridge.go`、`server/workerapp.go`、`server/session.go` 及 `server/*_test.go`。
- **关键改动与防坑点**：
  1. **修复 `libghostty` 字节上限语义（`server/session.go`）**：
     - 核查 `libghostty` Go 封装行为：`WithMaxScrollbackBytes(math.MaxUint)` / `SetScrollbackMaxBytes(&unlimited)` 才是解除字节限制；`SetScrollbackMaxBytes(nil)` 会恢复默认 `10,000` 字节限制。
     - 对 `scrollback > 0` 与 `scrollback == 0` 两条路径，创建与运行期调整均对 `s.grid` 和 `s.shadow` **同时**解除字节限制，并分别设置 `WithMaxScrollbackLines(uint(n))` / `SetScrollbackMaxLines(&lines)` 或不限行 / `SetScrollbackMaxLines(nil)`。
     - 保持 `s.gridMu` 保护，避免与任何外层锁冲突。
  2. **`settings.json` 解析边界与原子写（`server/manager.go` 或同包配置逻辑）**：
     - 校验区间为 `[0, 1000000]`，缺省值 `10000`。
     - JSON 解码必须用指针 `*int` 区分 `{}` 缺省与显式 `0`，**严禁**将 `{}` 零值误判为 `0`（无限）；`scrollback == nil`、浮点数/非法类型或超范围均视作损坏。
     - 仅 server 在启动损坏时备份为 `settings.json.corrupt-<unixNano>` 并回退 `10000`，写入采用 `CreateTemp` + `Chmod(0o600)` + `Rename`；worker 启动读取遇损坏仅记日志并回退 `10000`，绝不修改文件。
  3. **HTTP 接口与并发串行化（`server/main.go`、`server/handlers.go`、`server/manager.go`）**：
     - 在 `(m *manager).handler()` 注册 `GET /api/settings` 与 `PUT /api/settings`，复用既有 `readBody`、`writeJSON` 与 `CrossOriginProtection`。
     - `PUT /api/settings` 严格校验 JSON（拒绝缺失字段、`null`、非整数、越界或畸形 JSON → `400`）；使用独立于 `m.mu` 的序列化锁保护「写盘 + 更新内存值 + `scanWorkers(false)` + 遍历下发在线 worker」，写盘失败返回 `500` 且不改内存、不广播。
     - `manager.create()` 在 worker 就绪并将 handle 放入 `m.sessions` 时，若生效 `scrollback` 在拉起前后可能发生变化，补发一次当前配置同步。
  4. **Worker `set_scrollback` 握手与旧版兼容（`server/proto.go`、`server/bridge.go`、`server/workerapp.go`）**：
     - 在 `handshakeReq` 增加 `Scrollback *int`（`json:"scrollback,omitempty"`）；新增 `op: "set_scrollback"` 走 `workerOp(..., 2*time.Second)`。
     - `workerHost` 处理 `set_scrollback` 时须注意锁序：先在 `w.mu` 下校验并读取 `session`/`stopping`/`claimed` 或暂存未认领配置后释放 `w.mu`，再由 `session` 方法在 `s.gridMu` 内调整双网格（避免锁序反转或持 `w.mu` 阻塞）。
     - 准确区分 `staleSocket`（清死 handle，不计 `failed`）、旧版 worker 明确拒绝（`!ack.OK` 且 `ack.Op == "set_scrollback"`、`ack.UUID == id` 归为 `unsupported`，保留会话句柄）与通信/非预期失败（计入 `failed` 并记日志）。
- **完成证据**：带 `PKG_CONFIG_PATH` 执行 `go test -race -count=1 ./server` 全部通过（含新增覆盖 Spec 009 §6.1 全部要点的单测）。

### U2：前端界面交互、实时联动、`limpet_embed` 端到端验收与收尾清理

- **目标**：在终端工具弹窗上线用户可填的回滚行数设置与 `0 = 近似无限` 提示，完成前后端状态同步，并通过真实内嵌二进制的浏览器回归与专项验收。
- **涉及文件**：`web/src/sessions.ts`、`web/src/App.vue`、`web/src/components/TerminalView.vue`、`web/src/style.css`（及仅当既有测试选择器或覆盖确需调整时的 `tests/limet-reconnect-browser-test.mjs`）。
- **关键改动与防坑点**：
  1. **设置状态源与弹窗控件（`web/src/sessions.ts` / `App.vue` / `TerminalView.vue` / `style.css`）**：
     - 通过既有的 `request()` 封装在应用初始化拉取 `GET /api/settings`（自动保留 Cloudflare Access `opaqueredirect` → `authExpired` 处理），不使用 `localStorage` 存两份配置。
     - 在 `#terminal-tools` 弹窗字号行下方加入原生 `<form>` 数字输入框（`min="0"`、`max="1000000"`、`step="1"`、`inputmode="numeric"`）+ 「应用」按钮，**不设固定档位按钮**；弹窗打开（`@toggle` `newState === 'open'`）时主动刷新一次最新服务端设置并同步未编辑态草稿。
     - 在输入行下方常驻简洁小字提示说明 `0` 为近似无限（映射 100 万行）以及历史过多可能卡顿，当当前草稿值或生效值为 `0` 时应用 `--limpet-warn`；保存成功与报错内联展示，`unsupported > 0` / `failed > 0` 时如实提示。保持 `360px` 零横向溢出。
  2. **`xterm.js` 映射、动态生效与焦点保护（`web/src/components/TerminalView.vue`）**：
     - 统一映射函数：`0 → 1000000`，正整数保持原值；`new Terminal({ ..., scrollback: mappedScrollback })` 使用生效映射值，且当初始化拉取完成或保存成功时立即同步更新 `term.options.scrollback`（不触发重连或 `resize` 上报）。
     - 在 `openTerminal()` 的自动聚焦判断中除 `composerInput` 与 `<dialog open>` 外，补充检查 `.terminal-popover`（含 `#terminal-tools`）内的输入/活动焦点，确保重连不抢弹窗内编辑焦点。
  3. **真实内嵌编译、浏览器验收与清理（`LIMPET_HOME=~/.limpet-dev`）**：
     - 执行 `cd web && pnpm build`，带 `PKG_CONFIG_PATH` 编译 `go build -tags limpet_embed -o .runtime/limpet-server-009 ./server`。
     - 用该二进制启动独立 `LIMPET_HOME=~/.limpet-dev` 测试服务，执行 `tests/limet-reconnect-browser-test.mjs` 验证既有重连/移动场景全通过，并执行 Spec 009 §6.2 专项 Playwright 验收（涵盖 `>10 KiB` 多行历史跨刷新可回滚、`0` 映射 `1000000` 与警告提示、非法输入与旧 worker 提示、弹窗聚焦重连保活、`360px` 无溢出）。
     - 仅关闭本轮验收创建的临时 dev 会话 UUID，用普通 `kill` 停掉本轮临时测试 server，删除 `.runtime/limpet-server-009` 与临时验收脚本，保持工作区干净。
- **完成证据**：`cd web && pnpm build`、`go test -race -count=1 ./server`、既有与 Spec 009 专项 Playwright 验收全部通过，仓库干净无残留。

---

## GPT 执行与汇报要求

1. 严格按 `U1 → U2` 顺序实施；遵循 `AGENTS.md`，每完成一个可提交工作单元自动 `commit`（英文 Conventional Commits + 真实执行的 agent/模型 `Co-authored-by` 尾注，拿不准先向用户确认），**禁止主动 push**、**禁止触碰 `~/.limpet`**、**禁止执行 `deploy/install.sh`**，停自启临时 server 只用普通 `kill`。
2. 执行与最终汇报遵循 [gpt-worker-handoff.md](file:///home/lightless/.gemini/config/rules/gpt-worker-handoff.md)：
   - **先给业务结论与验证状态**，再展开明细，严禁堆砌免责声明；
   - **所有非零异常、告警、跳过或未解析计数必须带总量分母**，并明确区分既有覆盖边界与本轮新增问题；
   - 遇到真实技术卡点或契约分叉时先用通俗语言说明原因、方案代价与推荐，未经同意不得擅自缩减范围；
   - 单项复测按最小缺口验证，不机械重跑无关全量测试；
   - 全部单元完成并收尾后，**主动提醒用户开启新的 Codex 会话**。
