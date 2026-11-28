# Plan 009：前端可配置回滚行数（含 0 = 近似无限）与前后端联动

- 日期：2026-11-28
- 对应 Spec：`docs/specs/009-2026-11-28-configurable-scrollback.md`（已同步 GPT 预审修订，待复审）
- 执行方式：DELEGATED（适合转交 GPT / Codex 实施与验收，按 `U1 → U2` 顺序推进）
- 约束基线：
  - **环境隔离与上线红线**：Go 单测用 `t.TempDir()`，U2 浏览器验收必须使用**本轮专用隔离临时 `LIMPET_HOME`**（如 `.runtime/dev-home-009`）与独立空闲端口，**禁止**共用或污染 `~/.limpet-dev`，**严禁**执行 `deploy/install.sh`、**严禁**触碰 `/opt/limpet` 与正式 `~/.limpet`；不要加入仅服务于测试的生产开关后门。
  - **工具链环境变量**：编译与运行 Go 测试需带 `PKG_CONFIG_PATH="$HOME/.local/opt/limpet-toolchain/install-1.0.1/usr/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"`。
  - **进程清理安全**：停掉自己起的临时测试 server 只用普通 `kill`（绝不使用按进程树整棵清理的方式）；`pgrep -f`/`pkill -f` 若使用需遵循 `[x]yz` 形式。
  - **Git 规范**：遵循 `AGENTS.md`，每完成一个可提交单元自动 commit（英文 Conventional Commits，`Co-authored-by` 尾注按实际执行的 agent / 模型填写，拿不准先向用户确认，**不主动 push**）。

## 总体顺序与依赖关系

```text
U1（后端 settings.json 持久化、HTTP /api/settings、libghostty 字节上限修复、worker 启动读取/核对与运行期 set_scrollback 联动）
  └──▶ U2（前端回放前配置同步与背压水位、#terminal-tools 表单与变更重连、独立隔离环境 limpet_embed 验收与清理）
```

## 实施单元

### U1：后端配置持久化、HTTP 接口与 worker/双网格联动

- **目标**：落地 `<LIMPET_HOME>/settings.json` 读写与损坏恢复、`GET/PUT /api/settings` 串行化接口与精准统计广播、worker 启动读取/发布前重检与 `set_scrollback` 控制握手，并修复 `libghostty` 双网格默认 `10,000` 字节上限截断问题。
- **涉及文件**：`server/main.go`、`server/handlers.go`、`server/manager.go`、`server/proto.go`、`server/bridge.go`、`server/workerapp.go`、`server/session.go` 及 `server/*_test.go`。
- **关键改动与防坑点**：
  1. **修复 `libghostty` 双网格字节上限与页粒度语义（`server/session.go`）**：
     - 核查 `libghostty` 语义：`WithMaxScrollbackBytes(math.MaxUint)` / `SetScrollbackMaxBytes(&unlimited)` 才是解除字节上限；`SetScrollbackMaxBytes(nil)` 会恢复默认 `10,000` 字节限制；`SetScrollbackMaxLines(nil)` 则解除行数上限。
     - 对 `scrollback > 0` 与 `scrollback == 0`，创建与运行期更新均对 `s.grid` 与 `s.shadow` **同时**解除字节上限，并按需设置或解除行数上限。
     - 运行期调整在 `s.gridMu` 内先重查 `!s.ending.Load() && s.grid != nil && s.shadow != nil`，防止并发退出空指针；`libghostty` 按整页裁剪历史，单测应固定尺寸并用物理页/编号范围断言（避免把 `200` 当逐行硬上限）。
  2. **`settings.json` 解析边界、原子写与显式报错（`server/manager.go` 等）**：
     - 合法区间 `[0, 1000000]`，缺省值 `10000`。
     - JSON 解码必须使用 `*int` 区分 `{}` 缺失/`null` 与显式 `0`，并用完整 `json.Unmarshal` 拒绝尾随字符、小数、越界值。
     - server 启动读取沿用 `loadRegistry` 错误语义：非 `os.ErrNotExist` 的读取 I/O 失败或 `.corrupt-<unixNano>` 备份失败显式返回错误；原子写采用 `CreateTemp` + `Chmod(0o600)` + `Rename`。worker 只读加载，遇不可读或损坏仅记日志回退 `10000`，绝不修改文件。
  3. **HTTP 接口、串行化与创建竞态闭合（`server/main.go`、`server/handlers.go`、`server/manager.go`）**：
     - 注册 `GET /api/settings` 与 `PUT /api/settings`，复用 `readBody`（4 KiB 限制）、`writeJSON` 与 `CrossOriginProtection`。
     - `PUT /api/settings` 非法输入返回 `400` 且不动磁盘、内存或 worker；写盘失败返回 `500` 且不改内存、不广播。
     - 用独立于 `m.mu` 的 `settingsMu` 串行化「写盘 + 内存更新 + `scanWorkers(false)` + 复制 handle 并下发控制请求」，保持读取生效值与复制 handle 切片时 `m.mu` 仅极短加锁。
     - 精确分类广播结果：**仅** `ack.Op == "set_scrollback" && ack.UUID == id && !ack.OK && ack.Reason == "未知 worker 操作"` 计为 `unsupported`；其他 `!ack.OK`、`Op`/`UUID` 不符或非 `staleSocket` 错误计为 `failed` 并保留句柄；`staleSocket` 沿用 `dropStale` 清理，不计 `failed`。
     - `manager.create()` 拉起前记下 `before`，`probe` 就绪并登记到 `m.sessions`、清除 `pendingIDs` 之后，再核对当前生效值是否变化，变化则补发一次 `set_scrollback`。
  4. **Worker `set_scrollback` 握手、CLI 重检与迟到防回滚（`server/proto.go`、`server/bridge.go`、`server/workerapp.go`）**：
     - `handshakeReq` 增加 `Scrollback *int`（`json:"scrollback,omitempty"`）；`bridge.go` 提供支持传递完整 `handshakeReq` 的握手调用（保留原有 `probe`/`kill`/`attach`/`start` 报文不变）。
     - `workerHost` 收到 `set_scrollback` 时：`w.session == nil`（未认领 CLI worker 或启动中）或 `w.stopping` 直接回复 `OK: false`（`failed`，不设未认领暂存字段）；就绪态先释放 `w.mu` 再交由 `session` 进入 `s.gridMu`，并校验磁盘 `settings.json` 若存在合法新值且与 `*req.Scrollback` 不一致则以 `"配置已过期"` 拒绝，避免迟到旧 RPC 回滚新值。
     - CLI `start` 在 `w.session` 发布对 `probe` 可见后、首个 viewer `ftReady` 前，只读重检一次 `settings.json` 并在变化时更新双网格。
- **完成证据**：带 `PKG_CONFIG_PATH` 执行 `go test -race -count=1 ./server` 全部通过，覆盖 Spec 009 §6.1 全部单测项（含 `>10 KiB` 字节留存、页粒度调整、并发 `PUT`、`pendingIDs` 交错、迟到 RPC 防回滚、旧/新 worker 混合分类与 CLI 继承）。

### U2：前端同步、背压与变更重连、隔离 `limpet_embed` 验收与清理

- **目标**：在终端工具弹窗提供自由数字回滚配置与风险提示，落位回放前拉取与 `WriteBuffer` 背压水位、变更重连保 VT 状态与弹窗焦点保护，并在完全隔离的临时目录完成构建与浏览器端到端验收。
- **涉及文件**：`web/src/sessions.ts`（或共享设置状态）、`web/src/App.vue`、`web/src/components/TerminalView.vue`、`web/src/style.css`（不修改 `server/`，复用 U1 Go `-race` 测试结果）。
- **关键改动与防坑点**：
  1. **回放前读取设置、防乱序与表单交互（`web/src/sessions.ts`、`App.vue`、`TerminalView.vue`、`style.css`）**：
     - 通过统一 `request()` 拉取 `GET /api/settings`（保留 `opaqueredirect` → `authExpired` 识别），不使用 `localStorage`。用单调版本号丢弃过期 `GET`，防止迟到 `GET` 覆盖已生效值或正在编辑的草稿。
     - 终端 `connect()` 在建立 WebSocket 之前先执行有界 `GET /api/settings`（如 `2s` 超时，失败回退页面最近成功值或 `10000`），确保先拿配置后开连收 `ready` 与回放，也同步他页修改。
     - `#terminal-tools` 字号行下添加 `<form novalidate>` 数字输入框（`min="0"`、`max="1000000"`、`step="1"`、`inputmode="numeric"`，无档位按钮）与「应用」按钮；监听输入 `invalid` 与提交逻辑统一做内联校验。
     - 常驻小字明示 `0 表示近似无限（浏览器上限 100 万行）；设为无限且历史过多时，可能导致内存升高、重连变慢或页面卡顿`（输入或生效为 `0` 时用 `--limpet-warn`）；保存响应若同时含 `unsupported > 0` 和 `failed > 0` 时同时展示两类计数。保持 `360px` 弹窗与页面零横向溢出。
  2. **变更重连保 VT 状态、`WriteBuffer` 16 MiB 水位与焦点保护（`web/src/components/TerminalView.vue`）**：
     - 映射函数：`0 → 1000000`，正整数保持原值；仅在保存后**生效值发生变化**时调用一次 `connect()` 重建终端实例（严禁对活跃终端直接修改 `term.options.scrollback`，防止 `BufferSet.resize()` 重置 `DECSTBM`/Tab stops），未变化时不重连。
     - `drain()` 维护在途写字节计数器（高水位 `16 MiB`），在 `terminal.write(chunk, cb)` 回调减扣并在达到水位时暂停继续提交，避免大快照跨越 `xterm.js` `50,000,000` 字节 `DISCARD_WATERMARK` 崩溃。
     - `openTerminal()` 聚焦前检查 `:popover-open` 的 `.terminal-popover`、焦点是否在弹窗内或弹窗触发按钮上（以及 `composerInput` / `<dialog open>`），避免重连抢走打开弹窗或输入草稿的焦点。
  3. **独立隔离构建、浏览器验收与收尾清理**：
     - 执行 `cd web && pnpm build`，带 `PKG_CONFIG_PATH` 编译 `go build -tags limpet_embed -o .runtime/limpet-server-009 ./server`。
     - 使用**专用临时目录**（`LIMPET_HOME="$PWD/.runtime/dev-home-009"`）与专用空闲端口：
       - 由 `tests/limet-reconnect-browser-test.mjs` 按既有方式自行起停 server（传 `SERVER_BIN`、`LIMPET_HOME`、`PORT`），确认 **10/10 既有场景**通过；
       - 运行 Spec 009 §6.2 专项 Playwright 验收（独立起停临时目录内 server）：覆盖默认提示与 `360px` 零溢出、延迟 `GET` 时大历史重放不丢行、变更时保 `DECSTBM`/Tab stops 与滚动条同步、未改值不重连、`0` 映射 `1000000` 与真实重启 server 持久化、超 `8 MiB`/`16 MiB` 背压流控不崩溃、非法校验与混合提示、`WS` 重连期间弹窗焦点与草稿保留。
     - 验收结束后用普通 `kill` 停掉本轮临时测试 server，彻底删除 `.runtime/dev-home-009`、`.runtime/limpet-server-009` 与本轮临时验收脚本（不触碰共享 `~/.limpet-dev` 与 `~/.limpet`），确认仓库干净且 U2 不留 `server/` 改动。
- **完成证据**：`cd web && pnpm build` PASS、U1 `go test -race -count=1 ./server` 基准有效、既有 `10/10` 与 Spec 009 §6.2 专项 Playwright 全部通过，仓库干净无残留。

---

## GPT 执行与汇报要求

1. 严格按 `U1 → U2` 顺序实施；遵循 `AGENTS.md`，每完成一个可提交工作单元自动 `commit`（英文 Conventional Commits + 真实执行的 agent/模型 `Co-authored-by` 尾注，拿不准先向用户确认），**禁止主动 push**、**禁止触碰 `~/.limpet`**、**禁止执行 `deploy/install.sh`**，停自启临时 server 只用普通 `kill`。
2. 执行与最终汇报遵循 [gpt-worker-handoff.md](file:///home/lightless/.gemini/config/rules/gpt-worker-handoff.md)：
   - **先给业务结论与验证状态**，再展开明细，严禁堆砌免责声明；
   - **所有非零异常、告警、跳过或未解析计数必须带总量分母**，并明确区分既有覆盖边界与本轮新增问题；
   - 遇到真实技术卡点或契约分叉时先用通俗语言说明原因、方案代价与推荐，未经同意不得擅自缩减范围；
   - 单项复测按最小缺口验证，不机械重跑无关全量测试；
   - 全部单元完成并收尾后，**主动提醒用户开启新的 Codex 会话**。
