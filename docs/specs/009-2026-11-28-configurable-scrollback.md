# Spec 009：前端可配置回滚行数（含 0 = 近似无限）与前后端联动

- 日期：2026-11-28
- 状态：草案（待 GPT 预审）
- 来源：用户在 Web 端希望将终端回滚（scrollback）行数做到前端配置中直接修改，自由填写数字（不设固定档位按钮），默认保持 `10000`，填写 `0` 代表近似无限滚动（界面小字提示可能卡顿），并使后端 worker 与前端实时联动、重启不丢；同时排查 `server/session.go` 时发现既有缺陷——`libghostty.NewTerminal` 仅传了 `WithMaxScrollbackLines(10000)` 而未解除默认字节上限 `10,000` 字节，导致实际历史仅保留约 1 页即被字节上限截断，本 Spec 一并修复。
- 关联：延续并更新 [Spec 003](003-2026-11-23-libghostty-worker-host.md)、[Spec 006](006-2026-11-26-local-cli-attach.md) 与 [Spec 008](008-2026-11-28-xtermjs-webgl-frontend.md)；不改录制 `.log`/`.meta` 格式，不改 WebSocket 终端 I/O 与控制帧协议。

## 目标

1. **Web 端直改全局回滚行数**：在前端终端工具弹窗提供自由数字输入框（默认 `10000`，`0` 表示近似无限，无固定档位按钮），提交后持久化到服务端并立即联动更新当前终端、运行中的新版 worker 及后续新建会话。
2. **修复 `libghostty` 字节上限隐式截断**：在 `grid` 与 `shadow` 双网格创建及运行期更新时同步显式解除 `MaxScrollbackBytes` 默认 `10,000` 字节上限，使配置的行数上限或无限模式真实生效。
3. **无损兼容在线旧版 worker**：部署升级后仍存活的旧版 worker 进程不重启、不断线、不报错，能够准确统计并如实向用户反馈。

## 一、回滚行数语义与前后端映射契约

### 1.1 合法范围与默认值

- **合法范围**：闭区间整数 `[0, 1000000]`；未配置时的默认值为 `10000`。
- **非法输入**：负数、小数、布尔值、字符串、`null`、缺失字段以及超过 `1000000` 的数值一律视为非法，必须明确拒绝。
- **`0` 的语义**：`0` 专指「近似无限」模式（而非关闭回滚）。

### 1.2 后端 `libghostty` 双网格语义

`s.grid`（主/备全量网格）与 `s.shadow`（主屏影子网格）必须始终使用同一回滚配置：

- **正整数 `n ∈ [1, 1000000]`**：
  - 解除字节上限：创建时传入 `WithMaxScrollbackBytes(math.MaxUint)`；运行期更新时调用 `SetScrollbackMaxBytes(&unlimited)`（其中 `unlimited := uint(math.MaxUint)`；注意 `libghostty` 中传 `nil` 会恢复 `10,000` 字节默认上限而非解除限制）。
  - 启用行数上限：创建时传入 `WithMaxScrollbackLines(uint(n))`；运行期更新时调用 `SetScrollbackMaxLines(&lines)`。
- **无限模式 `n == 0`**：
  - 解除字节上限：同上，创建 `WithMaxScrollbackBytes(math.MaxUint)` / 运行期 `SetScrollbackMaxBytes(&unlimited)`。
  - 解除行数上限：创建时不传 `WithMaxScrollbackLines`（`nil` 默认无行数上限）；运行期更新时调用 `SetScrollbackMaxLines(nil)`。
- **运行期变更边界**：
  - 调大上限（含设为 `0`）仅对后续新滚动出去的输出生效，**不能**找回此前已被 `libghostty` 回收丢弃的历史行。
  - 调小上限会按 `libghostty` 内存页粒度立即裁剪超出新上限的历史页。

### 1.3 前端 `xterm.js` 映射语义

- 在 `xterm.js` 中，`scrollback: 0` 表示禁用滚动历史（0 行），且其内部环形缓冲区按 `new Array(rows + scrollback)` 分配定长数组，不能直接传入 `Number.MAX_SAFE_INTEGER`。
- 前端建立如下确定性映射：
  - `scrollback ∈ [1, 1000000]` → `xterm.js` `scrollback = n`；
  - `scrollback == 0` → `xterm.js` `scrollback = 1000000`（保证「近似无限」的可用缓冲上限不低于任何合法正整数配置，且避免超大数组分配导致浏览器异常）。

## 二、服务端持久化与 HTTP API 契约

### 2.1 `<LIMPET_HOME>/settings.json` 存储与恢复

- **文件路径与权限**：`<LIMPET_HOME>/settings.json`（默认 `~/.limpet/settings.json`，与 `sessions.json` 同级，新建权限 `0600`）。
- **最小载荷**：`{"scrollback": 10000}`。
- **单写者与原子写**：仅 `limpet-server` 可写入该文件（同目录临时文件 + `rename` 原子替换）；worker 进程对 `settings.json` 严格只读。
- **缺失与损坏语义**：
  - 文件不存在时，server 与 worker 均默认采用 `10000`（不强制自动创建文件）。
  - 解析 JSON 时必须使用指针字段或显式存在性检查区分「字段缺失/`null`」与「显式 `"scrollback": 0`」：**绝不允许**把 `{}` 的 Go 结构体零值误当成 `0`（近似无限）。字段缺失、`null`、非整数类型或超出 `[0, 1000000]` 一律判定为损坏。
  - `limpet-server` 启动读取到损坏 `settings.json` 时，记录日志，将损坏文件按 `settings.json.corrupt-<unixNano>` 备份移走（与 `sessions.json` 一致），内存回退默认值 `10000`。
  - `worker` 启动读取到损坏或不可读 `settings.json` 时，仅记录日志并回退 `10000`，**严禁**修改、删除或重命名该文件。

### 2.2 HTTP API（受既有同源保护与 4 KiB 请求体上限约束）

- **`GET /api/settings`**：
  - 返回 `200 OK`，JSON：`{"scrollback": <int>}`（返回当前内存生效值）。
- **`PUT /api/settings`**：
  - 请求体 JSON：`{"scrollback": <int>}`。
  - **严格校验**：字段缺失、`null`、小数、负数、非整数、大于 `1000000` 或非法 JSON / 多余非法体均返回 `400 Bad Request` `{"error": "..."}`，**不修改**内存值、磁盘文件或任何在线 worker。
  - **持久化优先与串行化**：
    - `PUT /api/settings` 的「写盘 + 内存更新 + 扫描并广播在线 worker」整段操作必须使用独立互斥锁串行化（**不复用**会话注册表锁 `m.mu`，避免持锁期间跨进程 socket I/O 阻塞 `/api/term`、创建或关闭请求），防止并发 `PUT` 导致磁盘值与 worker 最终状态乱序倒挂。
    - 写盘失败返回 `500 Internal Server Error` `{"error": "..."}`，保持原内存值且不向 worker 广播。
  - **广播与统计**：
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
      - `applied`：确认成功更新的新版在线 worker 数；
      - `unsupported`：明确返回不支持该控制操作的旧版在线 worker 数；
      - `failed`：因超时或非 `staleSocket` 错误未能确认更新的在线 worker 数（服务端记日志，已持久化的配置不回滚）；
      - 已确认失效的死 socket（`staleSocket`）沿用既有机制清理（`dropStale`），不计入 `failed`。
  - **幂等性**：重复 `PUT` 相同合法值时幂等返回 `200 OK` 并再次下发给当前在线 worker。

## 三、Worker 启动继承、运行期控制与兼容契约

### 3.1 启动读取与竞态闭合

- 新版 worker 启动（同时覆盖 Web `direct` / `systemd-run` 与 `limpet` CLI `start` 路径）创建 `grid` 与 `shadow` 时，直接以只读方式读取 `<LIMPET_HOME>/settings.json` 获取初始 `scrollback`（不新增环境变量污染用户 Shell，也不依赖 `systemd-run` 传额外 fd）。
- 为闭合 `PUT /api/settings` 与 `manager.create()` 刚拉起 worker 尚未登记到 `m.sessions` 之间的并发窗口：Web 创建会话在 worker 就绪并写入 `m.sessions` 时，若期间生效配置可能更新，需补发一次当前配置同步（`set_scrollback`）。

### 3.2 Worker 握手扩展（`op: "set_scrollback"`）与旧版兼容

- 在既有 `handshakeReq` JSON 中增加可选字段 `scrollback`（`*int` + `omitempty`，不影响既有 `probe` / `kill` / `start` / `attach` 报文兼容性）。
- 新增操作 `op: "set_scrollback"`（单次短连接请求—应答，沿用 `2s` 读超时，不占 viewer）：
  - **新版 worker**：校验 `UUID` 与 `Scrollback != nil && 0 <= *Scrollback <= 1000000`；合法且会话正在运行（`session != nil && !stopping && !ending`）时在 `s.gridMu` 临界区内同步更新 `grid` 与 `shadow` 并回复 `OK: true`（注意锁顺序安全：严禁持 `workerHost.mu` 期间等待 `s.gridMu`）；对于尚在等待 `start` 认领的 CLI worker（`!claimed && !stopping`），暂存该配置并在 `start` 创建会话时生效后回复 `OK: true`；会话正在结束或参数非法回复 `OK: false` 与明确 `Reason`。
  - **旧版 worker 兼容**：运行中的旧版 worker 收到 `op: "set_scrollback"` 会走入既有 `default` 分支回复 `OK: false`、`Reason: "未知 worker 操作"` 并关闭探测连接，对运行中终端零影响。server 收到此类明确拒绝时（`!ack.OK` 且 `ack.Op == "set_scrollback"` 与 `ack.UUID == id` 且错误为不支持操作），必须记为 `unsupported`，**绝不能**当成 `staleSocket` 删除会话，也**不能**导致 `PUT /api/settings` 返回 `500`。

### 3.3 大快照重放与传输边界

- `serveViewer` 重放 `formatVT(grid/shadow)` 快照已按 `wsChunk`（`64 KiB`）分块写入 `ftOutput`，因此即便解除字节限制或设 `0` 后快照总体积超过 `8 MiB`，也不会触发 `readFrame` 的 `8 MiB` 单帧限制。
- 极端大快照带来的内存占用、重连传输耗时与浏览器解析卡顿属于设定「近似无限」时的固有代价，由前端界面明确提示用户自行权衡。

## 四、前端交互与状态同步契约

### 4.1 配置入口与控件外观

- **入口位置**：放在终端页右上角「终端工具」弹窗（`#terminal-tools`）内、字号调节行下方，复用既有暗黑表面样式与控件间距，确保 `360px` 移动端视口下弹窗及整页均无横向溢出。
- **表单控件**：
  - 使用原生 `<form>` + 明确 `<label>` + 数字输入框（`<input type="number" inputmode="numeric" min="0" max="1000000" step="1">`，带 `tabular-nums`）+ 提交按钮（「应用」），**不设固定档位快捷按钮**；
  - 触控设备上保证表单控件与提交按钮拥有清晰可点击热区且字号合理（`16px` 输入框避免 iOS 自动缩放）。
- **卡顿小字提示**：
  - 在输入框下方常驻一行简洁小字提示：`0 表示近似无限（上限 100 万行）；设为无限且历史过多时，重连与渲染可能出现卡顿`；
  - 当输入框当前值或已生效值为 `0` 时，该提示以警告色（`--limpet-warn`）高亮；其余正整数配置下使用次要文本色（`--limpet-muted`）。

### 4.2 状态加载、保存、反馈与焦点保护

- **唯一真相与加载**：
  - 回滚配置仅以服务端为唯一真相，**不使用** `localStorage` 双份缓存。
  - 应用启动后通过与会话列表一致的 `request()` 封装拉取 `GET /api/settings`（自动兼容 Cloudflare Access `opaqueredirect` → `authExpired`）；若初始化请求失败则回退默认 `10000`，不阻断打开终端。
  - 每次打开 `#terminal-tools` 弹窗时刷新一次服务端最新配置（仅在草稿未处于编辑中/刚打开时同步输入框值）。
- **校验与提交**：
  - 前端提交前先做整数校验（空、非数字、小数、`< 0`、`> 1000000` 时直接给出内联错误，不发请求）；提交中禁用提交按钮防重复发送。
  - 若期间用户已打开终端，且 `GET /api/settings` 晚于 `openTerminal()` 返回或用户刚刚 `PUT` 成功，必须立即把映射后的 `scrollback` 应用到当前活跃 `Terminal` 实例（更新 `term.options.scrollback`，不触发重建连接或向后端发送 `resize`）；后续新建或重连实例直接使用最新生效值。
- **内联结果反馈**：
  - 保存成功展示简短状态文案：全部成功时显示 `已保存`；若存在旧版在线 worker（`unsupported > 0`）或更新失败（`failed > 0`），如实附带提示（如 `已保存（N 个旧版会话需新建后生效）` / `已保存（M 个会话未同步）`）。
  - 请求返回 `400`/`500` 或网络异常时，显示服务端返回的错误信息（或请求失败原因），保留输入框内容供修正，且**不改动**已生效的 `scrollback`。
- **重连焦点保护（延续 Spec 005/008）**：
  - 终端收到 `ready` 或完成重连聚焦时，若当前焦点在 `#terminal-tools`（或任何已打开的 `.terminal-popover` / 输入元素）或 `<dialog open>` 内，**绝不**将焦点抢回终端画布。

## 五、非目标

- **不做磁盘按行归档与惰性分页历史视图**：超过当前配置上限后历史仍由内存环丢弃，按需翻阅磁盘 `.log` 的惰性历史查看留待后续独立 Spec。
- **不做跨浏览器的 WebSocket 实时配置推送**：多端同时打开时，新打开终端或重新打开「终端工具」弹窗会刷新最新服务端设置，已开启的其他标签页不额外建设置推送通道。
- **不做会话级独立覆盖或通用设置中心**：仅服务全局 `scrollback` 这单一设置，不引入过度泛化的动态 schema/注册表框架。
- **不重启既有在线旧 worker**。

## 六、验收标准

### 6.1 后端 Go 单测（`go test -race -count=1 ./server`）

1. **字节上限修复验证**：向新建会话或运行中 `10000` 行 `session` 写入超过 `10,000` 字节但总行数低于 `10,000` 行（如 `2,000` 行、`~160 KiB`）的输出，验证 `grid` 与 `shadow` 均完整保留全部逻辑历史且无字节截断。
2. **`0`（近似无限）保留验证**：配置为 `0` 后写入 `> 12,000` 行输出，验证 `grid` 与 `shadow` 均保留超过 `10,000` 行的历史（不回退到默认 `10,000` 行或 `10,000` 字节）。
3. **运行期动态调整（含 `0`）**：
   - 运行中经 `set_scrollback` 从 `10000 → 200`：调小后双网格历史均被裁剪至受控页范围内，再写新输出按 `200` 行上限循环覆盖；
   - 运行中从 `200 → 5000` 与 `5000 → 0`：`grid` 与 `shadow` 同步生效且原有历史保留不被清空。
4. **持久化、解析边界与并发串行化**：
   - 缺失 `settings.json` 时默认 `10000`；
   - 存在 `{"scrollback": 0}` 时精确读取并重启保留 `0`；存在 `{}` 或 `{"scrollback": null}` 或越界/负数时，server 启动备份为 `.corrupt-*` 并回退 `10000`，worker 只读回退 `10000` 且不改坏文件；
   - `PUT /api/settings` 拒绝负数、小数、`> 1000000`、缺失字段与 `null`（`400`），写盘失败返回 `500` 且不改内存值；并发 `PUT` 串行执行不与最终落盘值倒挂。
5. **新旧 worker 混合与启动继承**：
   - 混合接入支持与模拟仅回复 `"未知 worker 操作"` 的旧版 worker socket，验证 `PUT /api/settings` 返回 `200`，`applied`/`unsupported`/`failed` 计数准确且不删除旧 worker socket；
   - Web 新建与 CLI `start`（含尚未 `start` 认领前收到 `set_scrollback`）均正确继承配置的 `scrollback`。

### 6.2 前端构建与真实内嵌二进制 Playwright 验收（`LIMPET_HOME=~/.limpet-dev`）

1. **构建零错误**：`cd web && pnpm build`（含 `vue-tsc -b`）通过，`go build -tags limpet_embed` 成功。
2. **UI 与默认提示**：打开「终端工具」弹窗，默认值为 `10000`，输入框下方显示 `0` = 近似无限及卡顿风险提示小字，无预设档位按钮；`360px` 视口下整页与弹窗无横向溢出。
3. **正整数配置联动与跨重载验证**：在弹窗将回滚行数改为 `20000` 并提交成功；向会话写入超 `12,000` 行（且总字节远超 `10,000` 字节）的带行号输出后刷新页面重连，验证 `xterm.js` 仍可回滚查看最早行（证明前端映射、worker 字节上限解除与重连快照完整联动）。
4. **设为 `0`（近似无限）与警告高亮**：修改为 `0` 提交成功，提示小字切换警告样式（`--limpet-warn`）；写入 `> 15,000` 行输出后重载，验证历史完整保留且 `term.options.scrollback === 1000000`，服务端 `GET /api/settings` 仍返回 `0`。
5. **非法拦截、旧 worker 提示与焦点保护**：输入超上限（如 `1000001`）或负数时阻断并显示错误且不改变生效配置；模拟 `unsupported > 0` 返回时正确显示旧版会话提示；打开「终端工具」弹窗编辑输入框期间触发 WS 重连，验证焦点不被抢回终端画布。
6. **既有重连回归无退化**：运行 `tests/limet-reconnect-browser-test.mjs` 全部既有场景通过。
