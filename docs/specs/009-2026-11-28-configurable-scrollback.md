# Spec 009：前端可配置回滚行数（含 0 = 近似无限）与前后端联动

- 日期：2026-11-28
- 状态：已修订（完成 GPT 预审修订，待复审）
- 来源：用户在 Web 端希望将终端回滚（scrollback）行数做到前端配置中直接修改，自由填写数字（不设固定档位按钮），默认保持 `10000`，填写 `0` 代表近似无限滚动（界面小字提示可能卡顿），并使后端 worker 与前端实时联动、重启不丢；同时排查 `server/session.go` 时发现既有缺陷——`libghostty.NewTerminal` 仅传了 `WithMaxScrollbackLines(10000)` 而未解除默认字节上限 `10,000` 字节，导致实际历史仅保留约 1 页即被字节上限截断，本 Spec 一并修复。
- 关联：延续并更新 [Spec 003](003-2026-11-23-libghostty-worker-host.md)、[Spec 006](006-2026-11-26-local-cli-attach.md) 与 [Spec 008](008-2026-11-28-xtermjs-webgl-frontend.md)；不改录制 `.log`/`.meta` 格式，不改 WebSocket 终端 I/O 与控制帧协议。

## 目标

1. **Web 端直改全局回滚行数**：在前端终端工具弹窗提供自由数字输入框（默认 `10000`，`0` 表示近似无限，无固定档位按钮），提交后持久化到服务端，并同步更新当前终端、运行中的新版 worker 及后续新建会话。
2. **修复 `libghostty` 字节上限隐式截断**：在 `grid` 与 `shadow` 双网格创建及运行期更新时同步显式解除 `MaxScrollbackBytes` 默认 `10,000` 字节上限，使配置的行数上限或无限模式真实生效。
3. **无损兼容在线旧版 worker**：部署升级后仍存活的旧版 worker 进程不重启、不断线、不报错，能够准确统计并如实向用户反馈。

## 一、回滚行数语义与前后端映射契约

### 1.1 合法范围与默认值

- **合法范围**：闭区间整数 `[0, 1000000]`；未配置时的默认值为 `10000`。
- **非法输入**：负数、小数、布尔值、字符串、`null`、缺失字段以及超过 `1000000` 的数值一律视为非法，必须明确拒绝。
- **`0` 的语义**：`0` 专指「近似无限」模式（而非关闭回滚）。

### 1.2 后端 `libghostty` 双网格语义

`s.grid`（主/备全量网格）与 `s.shadow`（主屏影子网格）必须始终使用同一回滚配置（注：备用屏本身不产生 scrollback，以下配置约束各自的主屏历史）：

- **正整数 `n ∈ [1, 1000000]`**：
  - 解除字节上限：创建时传入 `WithMaxScrollbackBytes(math.MaxUint)`；运行期更新时调用 `SetScrollbackMaxBytes(&unlimited)`（其中 `unlimited := uint(math.MaxUint)`；注意 `libghostty` 中传 `nil` 会恢复 `10,000` 字节默认上限而非解除限制）。
  - 启用行数上限：创建时传入 `WithMaxScrollbackLines(uint(n))`；运行期更新时调用 `SetScrollbackMaxLines(&lines)`。
- **无限模式 `n == 0`**：
  - 解除字节上限：同上，创建 `WithMaxScrollbackBytes(math.MaxUint)` / 运行期 `SetScrollbackMaxBytes(&unlimited)`。
  - 解除行数上限：创建时不传 `WithMaxScrollbackLines`（`nil` 默认无行数上限）；运行期更新时调用 `SetScrollbackMaxLines(nil)`。
- **页粒度与运行期变更边界**：
  - `libghostty` 按整页（page）粒度裁剪物理历史行，且至少保留一标准页，跨活动区边界的历史页也会保留；因此设定行数为近似物理上限而非精确逐行硬截断。
  - 调大上限（含设为 `0`）仅对后续新滚动出去的输出生效，**不能**找回此前已被 `libghostty` 回收丢弃的历史行。
  - 调小上限会按整页粒度立即回收符合条件的超限历史页。

### 1.3 前端 `xterm.js` 映射语义

- 在 `xterm.js` 中，`scrollback: 0` 表示禁用滚动历史（0 行），且其内部环形缓冲区按 `new Array(rows + scrollback)` 分配定长数组，不能直接传入 `Number.MAX_SAFE_INTEGER`。
- 前端建立如下确定性映射：
  - `scrollback ∈ [1, 1000000]` → `xterm.js` `scrollback = n`；
  - `scrollback == 0` → `xterm.js` `scrollback = 1000000`（明确标为「浏览器上限 100 万行」，保证其可用缓冲不低于任何合法正整数配置，且避免极大数组分配导致浏览器异常）。

## 二、服务端持久化与 HTTP API 契约

### 2.1 `<LIMPET_HOME>/settings.json` 存储与恢复

- **文件路径与权限**：`<LIMPET_HOME>/settings.json`（默认 `~/.limpet/settings.json`，与 `sessions.json` 同级，新建权限 `0600`）。
- **最小载荷**：`{"scrollback": 10000}`。
- **单写者与原子写**：仅 `limpet-server` 可写入该文件（同目录临时文件 + `rename` 原子替换）；worker 进程对 `settings.json` 严格只读。
- **缺失与损坏恢复语义**：
  - 文件不存在（`os.ErrNotExist`）时，server 与 worker 均默认采用 `10000`（不强制自动创建文件）。
  - 解析 JSON 时必须区分「字段缺失/`null`」与「显式 `"scrollback": 0`」：**绝不允许**把 `{}` 的 Go 结构体零值误当成 `0`（近似无限）。字段缺失、`null`、非整数类型、尾随非法内容或超出 `[0, 1000000]` 一律判定为损坏。
  - **server 沿用 `loadRegistry` 显式报错语义**：若读取失败且错误不是 `os.ErrNotExist`，或损坏文件备份重命名（`settings.json.corrupt-<unixNano>`）失败，必须明确返回启动错误，不静默吞错；JSON 损坏且备份成功后记录日志并回退 `10000`。
  - **worker 只读回退**：worker 启动读取到损坏或不可读 `settings.json` 时，仅记录日志并回退 `10000`，**严禁**修改、删除或重命名该文件。

### 2.2 HTTP API（受既有同源保护与 4 KiB 请求体上限约束）

- **`GET /api/settings`**：
  - 返回 `200 OK`，JSON：`{"scrollback": <int>}`（读取当前内存生效值）。
- **`PUT /api/settings`**：
  - 请求体 JSON：`{"scrollback": <int>}`。
  - **严格校验**：字段缺失、`null`、小数、负数、非整数、大于 `1000000` 或畸形/含尾随内容的请求体均返回 `400 Bad Request` `{"error": "..."}`，**不修改**内存值、磁盘文件或任何在线 worker。
  - **持久化优先与串行化**：
    - `PUT /api/settings` 的「写盘 + 内存更新 + 扫描并广播在线 worker」整段操作必须使用独立互斥锁串行化（**不复用**会话注册表锁 `m.mu`，避免跨进程 socket I/O 阻塞 `/api/term`、创建或关闭请求；读写内存生效值及复制 handle 列表时 `m.mu` 仅保持极短临界区），防止并发 `PUT` 导致磁盘值与 worker 最终状态乱序倒挂。
    - 写盘失败返回 `500 Internal Server Error` `{"error": "..."}`，保持原内存值且不向 worker 广播。
  - **广播与精确分类统计**：
    - 写盘并更新内存生效值后，先触发一次 `scanWorkers(false)` 发现尚未纳管的新在线 worker，再逐一通过 worker 控制握手下发新 `scrollback`。
    - 返回 `200 OK`，JSON：
      ```json
      {
        "scrollback": 50000,
        "applied": 2,
        "unsupported": 1,
        "failed": 0
      }
      ```
      - `applied`：`ack.OK && ack.Op == "set_scrollback" && ack.UUID == id` 的新版在线 worker 数；
      - `unsupported`：**仅当** `ack.Op == "set_scrollback" && ack.UUID == id && !ack.OK && ack.Reason == "未知 worker 操作"` 精确匹配旧版 worker 拒绝时计入；
      - `failed`：在线但返回其他拒绝理由（如正在结束、未就绪、配置已过期、参数异常）、应答 `Op`/`UUID` 不符、或发生超时及非 `staleSocket` 错误的 worker 数（服务端记日志，保留会话句柄不删除，不回滚已持久化配置，仍返回部分成功 `200 OK`）；
      - 已确认失效的死 socket（`staleSocket`）沿用既有机制清理（`dropStale`），不计入 `failed`。
  - **幂等性**：重复 `PUT` 相同合法值时幂等返回 `200 OK` 并再次下发给当前在线 worker。

## 三、Worker 启动继承、运行期控制与兼容契约

### 3.1 启动读取与 Web / CLI 竞态闭合

- 新版 worker 启动（同时覆盖 Web `direct` / `systemd-run` 与 `limpet` CLI `start` 路径）创建 `grid` 与 `shadow` 时，直接以只读方式读取 `<LIMPET_HOME>/settings.json` 获取初始 `scrollback`（不新增环境变量污染用户 Shell，也不依赖 `systemd-run` 传额外 fd）。
- **Web 创建登记与重同步顺序**：`manager.create()` 在拉起 worker 前先记录当时的内存生效值 `before`；待 worker `probe` 就绪后，先加短锁将新 handle 写入 `m.sessions` 并清除 `pendingIDs[id]`（使后续并发 `PUT` 广播能发现该会话），随后在 `m.mu` 外读取当前最新生效值；若当前值与 `before` 不同，向该 worker 补发一次 `set_scrollback`。
- **CLI `start` 启动窗口闭合（无未认领暂存）**：CLI `start` 在 `startSession()` 创建双网格时读取 `settings.json`；当 `w.session` 已发布使 `probe` 可见后、且向首个 viewer 发送 `ftReady` 之前，再只读核对一次 `<LIMPET_HOME>/settings.json`，若与网格创建时读到的值不同则立即更新双网格，从而闭合创建到发布之间的配置竞态窗口。

### 3.2 Worker 握手扩展（`op: "set_scrollback"`）、迟到 RPC 防护与旧版兼容

- 在既有 `handshakeReq` JSON 中增加可选字段 `scrollback`（`*int` + `omitempty`），不影响既有 `probe` / `kill` / `start` / `attach` 报文兼容性。
- 新增操作 `op: "set_scrollback"`（单次短连接请求—应答，沿用 `2s` 读超时，不占 viewer）：
  - **未就绪与停止态拒绝**：若 `w.session == nil`（尚在等待 CLI `start` 认领或创建中）或 `w.stopping == true`，立即释放 `w.mu` 并回复 `OK: false`（如 `"会话尚未就绪"` / `"会话正在结束"`），不再做复杂暂存；server 对该类应答归为 `failed`（不误记为 `unsupported`）。
  - **锁序安全与迟到防回滚**：在 `w.mu` 下取得 `session` 引用后立即释放 `w.mu`，再获取 `s.gridMu`；在 `s.gridMu` 内重查 `s.ending.Load() || s.grid == nil || s.shadow == nil`（若已结束或已释放回复 `OK: false`）。为防止前端/server 超时后的迟到旧 `set_scrollback` RPC 在晚于更新的 `PUT` 时把 worker 盖回旧值：在 `s.gridMu` 串行临界区内只读检查 `<LIMPET_HOME>/settings.json`，当存在合法配置且磁盘最新 `scrollback != *req.Scrollback` 时拒绝覆盖并回复 `OK: false`（原因 `"配置已过期"`）；否则同步更新 `grid` 与 `shadow` 并按双网格实际更新结果回复 `OK: true`（或错误详情）。
  - **旧版 worker 兼容**：运行中的旧版 worker 收到 `op: "set_scrollback"` 会走入既有 `default` 分支回复 `OK: false`、`Reason: "未知 worker 操作"` 并关闭探测连接，对运行中终端零影响。server 仅对此精确应答归类为 `unsupported`，**绝不能**当成 `staleSocket` 删除会话，也**不能**导致 `PUT /api/settings` 返回 `500`。

### 3.3 大快照重放与前端 `WriteBuffer` 背压边界

- `serveViewer` 重放 `formatVT(grid/shadow)` 快照已按 `wsChunk`（`64 KiB`）分块写入 `ftOutput`，不受 `readFrame` `8 MiB` 单帧上限阻断；后端不在快照流中途截断或直接拒绝重连（因为 VT 流从旧到新输出，中途截断会砍掉底部当前活动屏），极端大历史带来的服务端序列化等待、重连传输耗时与内存占用由前端界面提示用户自行权衡。
- **前端 `WriteBuffer` 背压**：`xterm.js` 的 `WriteBuffer` 在未解析积压超过 `50,000,000` 字节（`DISCARD_WATERMARK`）时会直接抛错触发终端进入 `fatalError`；因此前端 `drain()` 交付循环必须维护在途写字节水位（高水位 `16 MiB`）：向 `terminal.write(chunk, cb)` 提交的字节累计达到水位时暂停继续投喂后续待处理输出块，待写回调释放字节后再继续排空队列（回调内严格核对连接代数与活跃终端实例一致）。

## 四、前端交互与状态同步契约

### 4.1 配置入口与控件外观

- **入口位置**：放在终端页右上角「终端工具」弹窗（`#terminal-tools`）内、字号调节行下方，复用既有暗黑表面样式与控件间距，确保 `360px` 移动端视口下弹窗及整页均无横向溢出。
- **表单控件**：
  - 使用原生 `<form novalidate>` + 明确 `<label>` + 数字输入框（`<input type="number" inputmode="numeric" min="0" max="1000000" step="1">`，带 `tabular-nums`）+ 提交按钮（「应用」），**不设固定档位快捷按钮**；使用 `novalidate` 确保所有非法输入统一走前端内联错误展示，不被浏览器原生校验气泡静默拦掉 `submit`。
  - 触控设备上保持清晰可点击热区与 `16px` 输入框字号，避免 iOS 自动缩放。
- **卡顿小字提示**：
  - 在输入框下方常驻一行可换行小字提示：`0 表示近似无限（浏览器上限 100 万行）；设为无限且历史过多时，可能导致内存升高、重连变慢或页面卡顿`；
  - 当输入框当前值或已生效值为 `0` 时，该提示以警告色（`--limpet-warn`）高亮；其余正整数配置下使用次要文本色（`--limpet-muted`）。

### 4.2 状态加载、变更重连与焦点保护

- **唯一真相与回放前加载**：
  - 回滚配置仅以服务端为唯一真相，**不使用** `localStorage` 双份缓存。
  - 应用启动挂载时通过与会话列表一致的 `request()` 封装拉取 `GET /api/settings`（自动兼容 Cloudflare Access `opaqueredirect` → `authExpired`）。
  - 终端 `connect()` 在建立 WebSocket、接收 `ready` 与回放历史之前，必须先完成一次有界 `GET /api/settings` 读取（例如 `2s` 超时，失败/超时则回退页面最近一次成功加载值或默认 `10000`），从而保证：1）不会先按 `10000` 吃完大历史回放再丢掉本应保留的旧行；2）另一标签页修改设置后，新开或重连终端可立即拿到最新值。
  - 每次打开 `#terminal-tools` 弹窗时也刷新一次最新服务端设置；所有 `GET` 与 `PUT` 更新通过单调递增版本序号防乱序，过期 `GET` 响应不得覆盖已生效值，也不得覆盖用户正在编辑/未保存的草稿。
- **提交校验与变更重连生效（保留 VT 状态与滚动条同步）**：
  - 直接修改活跃 `xterm.js` 的 `term.options.scrollback` 会触发 `BufferSet.resize()`，从而重置 `DECSTBM` 滚动区与部分 Tab stops 且需手工同步滚动条；因此前端**不在运行中的 `Terminal` 上热改 `options.scrollback`**。
  - 提交前校验空值、非整数、`< 0` 或 `> 1000000`（并处理输入框 `invalid` 兜底）展示内联错误且不发请求；提交中禁用提交按钮防重入。
  - `PUT /api/settings` 成功更新生效值后：若生效值与当前活跃 `Terminal` 使用的配置**发生变化**，触发一次完整的 `connect()` 重建当前终端实例（与「锁定原宽」切换一致），以新配置重放完整 `libghostty` 快照并保持 `DECSTBM`/Tab stops 与滚动条完全正确；若提交后的生效值与当前已应用值**相同**，不触发无意义重连。
- **内联结果反馈**：
  - 保存成功展示简短状态文案：全部成功时显示 `已保存`；若存在 `unsupported > 0` 或 `failed > 0`（两者均大于 0 时必须**同时**展示），如实附带提示（如 `已保存（N 个旧版会话需新建后生效、M 个会话未同步）`）。
  - 请求返回 `400`/`500` 或网络异常时，显示服务端返回的错误信息（或请求失败原因），保留输入框内容供修正，且**不改动**已生效的 `scrollback`。
- **重连焦点保护（延续 Spec 005/008）**：
  - `openTerminal()` 完成连接聚焦前，若当前 `#terminal-tools` 或任何 `.terminal-popover` 处于打开状态（`:popover-open`），或当前焦点位于弹窗内部、弹窗触发按钮、`composerInput` 或 `<dialog open>` 内，**绝不**将焦点抢回终端画布。

## 五、非目标

- **不做磁盘按行归档与惰性分页历史视图**：超过当前配置上限后历史仍由内存环丢弃，按需翻阅磁盘 `.log` 的惰性历史查看留待后续独立 Spec。
- **不做跨浏览器的 WebSocket 实时配置推送**：多端同时打开时，新开/重连终端或重新打开「终端工具」弹窗会自动刷新最新服务端设置，不额外建配置广播通道。
- **不做会话级独立覆盖或通用设置中心**：仅服务全局 `scrollback` 这单一设置，不引入过度泛化的动态 schema/注册表框架。
- **不重启既有在线旧 worker**。

## 六、验收标准

### 6.1 后端 Go 单测（`go test -race -count=1 ./server`）

1. **字节上限修复验证**：向新建会话或运行中 `10000` 行 `session` 写入超过 `10,000` 字节但总行数低于 `10,000` 行（如 `2,000` 行、`~160 KiB`）的输出，验证 `grid` 与 `shadow` 均完整保留全部逻辑历史且无字节截断。
2. **`0`（近似无限）保留验证**：配置为 `0` 后写入 `> 12,000` 行带编号输出，验证 `grid` 与 `shadow` 均保留超过 `10,000` 行的历史（不回退到默认 `10,000` 行或 `10,000` 字节）。
3. **运行期动态调整（含 `0` 与页粒度边界）**：
   - 固定终端尺寸，在运行中经 `set_scrollback` 从 `10000 → 200` 写入足够多带编号行，验证双网格历史均被整页裁剪到合理有界物理页范围（允许页粒度余量，不硬断言逐行 `== 200`），且最早行已被丢弃；
   - 运行中从 `200 → 5000` 与 `5000 → 0`：`grid` 与 `shadow` 同步生效且当前保留的编号历史不被清空。
4. **持久化、解析边界与报错语义**：
   - 缺失 `settings.json` 时默认 `10000`；
   - 存在 `{"scrollback": 0}` 时精确读取并在重启后保留 `0`；存在 `{}`、`{"scrollback": null}`、小数、越界或负数时，server 启动备份为 `.corrupt-*` 并回退 `10000`，worker 只读回退 `10000` 且不改坏文件；读取 I/O 异常或备份失败时 server 显式返回启动错误；
   - `PUT /api/settings` 拒绝负数、小数、`> 1000000`、缺失字段与 `null`（`400`），写盘失败返回 `500` 且不改内存值。
5. **并发串行化、创建竞态与迟到 RPC 防护**：
   - 并发 `PUT` 串行化后磁盘最终值与在线 worker 生效值严格一致；
   - 验证 `manager.create()` 在 `pendingIDs` 期间被 `PUT` 扫描跳过、登记完成后补发并对齐新配置的确定性交错；
   - 验证落后于当前磁盘 `settings.json` 的迟到 `set_scrollback` RPC 被 worker 拒绝（不把已更新的 worker 改回旧值）。
6. **新旧 worker 混合与 CLI 启动继承**：
   - 混合接入支持的新版 worker、模拟仅回复 `"未知 worker 操作"` 的旧版 worker、回复其他拒绝理由的新版 worker（如未就绪/停止中）以及身份/操作不符的异常应答，验证 `PUT /api/settings` 返回 `200`，`applied`/`unsupported`/`failed` 精确分类且旧版与正常拒绝句柄均保留；
   - 验证 Web 新建与 CLI `start`（含发布前配置变更后的 `ftReady` 前重检，以及未就绪前收到 `set_scrollback` 返回非 `unsupported` 拒绝）均正确继承最新 `scrollback`。

### 6.2 前端构建与真实内嵌二进制 Playwright 验收（独立临时 `LIMPET_HOME` 与独立端口）

1. **构建与回归基准**：`cd web && pnpm build`（含 `vue-tsc -b`）通过，`go build -tags limpet_embed` 成功；在独立临时 `LIMPET_HOME` 与空闲端口下由 `tests/limet-reconnect-browser-test.mjs` 自行管理启停，既有 **10/10 场景**全部通过；U2 未修改 `server/` 时可复用 U1 通过的 Go `-race` 结果。
2. **UI、默认提示与 `360px` 布局**：打开「终端工具」弹窗，默认值为 `10000`，输入框下方显示 `0` = 近似无限（含浏览器上限 `100 万行` 与内存/重连卡顿风险）小字，无预设档位按钮；`360px` 视口下整页与弹窗无横向溢出。
3. **回放前读配置与防丢历史**：将服务端预先设为 `20000` 并注入超 `12,000` 行带编号（`> 10,000` 字节）历史；新开会话连接并刻意延迟 `GET /api/settings` 响应，验证客户端等待设置到位后再消费回放，最终编号历史完整且每行仅出现一次。
4. **改值重连保 VT 状态 & 未改值不重连**：在活跃终端先写入 `DECSTBM` 滚动区与自定义 Tab stops 标记，修改回滚行数并提交成功后，验证重建重连仍保有正确滚动区、Tab stops 行为及准确滚动条；再次提交相同数值时验证不触发重连；期间模拟迟到 `GET` 返回旧值时不覆盖当前生效值或编辑中草稿。
5. **设为 `0`、跨重启持久化与 `WriteBuffer` 背压**：在界面保存 `0`，确认小字高亮 `--limpet-warn` 且终端映射 `scrollback === 1000000`；真实重启 test server 后重新访问，`GET /api/settings` 仍返回 `0`；写入并回放超过 `8 MiB` 与前端 `16 MiB` 水位的超大带编号历史，验证平滑分流交付不触发 `WriteBuffer` 50 MB 崩溃且可回滚查看。
6. **校验阻断、混合提示与焦点保护**：输入空值、`-1`、`1000001` 时内联报错阻断提交；模拟同时返回 `unsupported > 0 && failed > 0` 时两项计数均明确展示；在 `#terminal-tools` 弹窗打开及输入框聚焦填写期间触发 WS 重连，验证重连后焦点未抢回画布且输入框草稿保留。
