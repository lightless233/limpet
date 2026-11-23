# Plan 002：终端持久历史

- 日期：2026-11-23
- 对应 Spec：`docs/specs/002-2026-11-23-persistent-history.md`
- 状态：**暂缓（hold）**——随 Spec 002 暂缓（宿主地基变更，见 Spec 003）。U1 的 uuid 身份思路由 Plan 003 吸收；U3（历史读取 API）与 U4（前端历史视图）待 003 落地后重启评审；U2（pipe-pane）随 tmux 退役废弃。
- 执行方式：INLINE，单人单会话
- 依赖顺序：U1 → U2 → U3 → U4 → U5（U2 与 U3 可交错；U4 依赖 U1 的 API 形状）

## 单元 1：稳定 ID 与注册表

**问题**：会话身份 = tmux 名（`server/term.go` rename 时 map 重键 + entry.id 更新），改名即断接续；死会话无任何来源。

**改动**：
- 创建路径分配 uuid：`set-option @limpet-id` → 注册表登记（name/createdAt）。创建返回体、`GET /api/term` 列表、WS 路由、close、rename 全部以 uuid 寻址；rename 只更新 tmux 名与注册表 name。
- uuid → tmux 定位（**禁止「uuid → 名字 → `-t =<名>`」两步寻址**）：单次 `list-sessions -F '#{session_name} #{session_id} #{@limpet-id}'` 按 `@limpet-id` 匹配，取 tmux 内部 session_id（`$N`）；接入、关闭、重命名一律以 `$N` 为目标，操作前核对目标会话 `@limpet-id` 与 uuid 一致（server 重启后 `$N` 会复用）。两步寻址的 TOCTOU 已隔离复现：查名后旧会话改名、同名新会话重建，按名迟到关闭会杀错会话。
- 注册表 `~/.limpet/sessions.json`：原子写 + 互斥；结束判定（首次观测不在 tmux 中 → endedAt）；close 与 exit 同路径。
- 列表响应（id/name/created/endedAt/attached/live/hasHistory），活在前、死后按 endedAt 倒序；`endedAt` 供前端展示结束时间。
- `DELETE /api/term/{id}`：仅死会话，删条目 + 历史文件；活会话 409。

**完成证据**：rename 后同一 uuid 可接入且注册表 name 已更新；**「改名 + 同名重建」窗口内关闭旧会话：命中的是旧会话（`$N` 定位），新同名会话不受影响**；close/exit 后列表出现含 endedAt 的死条目；同名再建 uuid 不同；DELETE 语义（活 409 / 死删除）。

## 单元 2：pipe-pane 记录通道

**问题**：没有任何磁盘记录；断开与离线期间的输出无处可寻。

**改动**：
- 创建时**同一 tmux 进程命令队列**（`;` 链接、全静态参数）下发 `new-session` + `set-option @limpet-id` + `pipe-pane`，**不分步执行**——分步会漏录 Shell 首条输出（隔离实测），同队列 20/20 录全。pipe 目标用创建名（`-t =<名>`，此刻不存在重命名窗口）。记录程序归属 tmux；启动失败不阻断创建，`hasHistory=false` 等重武装补上。
- 创建后立即 `display-message -p '#{pane_id}'` 读出原 pane ID 存 `@limpet-pane`。
- 重武装（硬约束：先查 `#{pane_pipe}`，为 0 才启动——3.3a 实测对活 pipe 二次 `pipe-pane -o` 会把它关掉）：Go 启动时 + 每次 `GET /api/term` 时，**按 `@limpet-pane` 恢复，绝不按活动 pane**（split 实测：活动 pane 会变，按它恢复会把另一 pane 的输出写进同一历史）；原 pane 已消失则不恢复；`@limpet-pane` 缺失时单 pane 推导、多 pane 不盲恢复。
- 存量迁移：无 `@limpet-id` 的活会话 → 分配 uuid → `capture-pane -p -e -S -50000 -E -1` 种子回填 → 按推导出的原 pane 启动 pipe。
- `tmuxBin()` 覆盖照常生效。

**完成证据**：创建后即时 echo 全部落盘（不漏首条）；浏览器断开后输出持续进文件；Go 重启（不杀 tmux）后记录继续且仍绑定原 pane；split 后重武装仍录原 pane、第二 pane 输出不混入；存量会话重启后被回填并可翻阅。

## 单元 3：历史读取 API

**问题**：磁盘上有字节流，没有读取路径。

**改动**：
- `GET /api/term/{id}/history?before&limit`，合同见 spec：LF 对齐行切分、ANSI 剥离（CSI/OSC/其他 ESC）、孤 `\r` 行内覆写、非法 UTF-8 → U+FFFD。
- 行索引：首次整扫建立 LF 偏移索引后常驻，此后**按已扫偏移增量扫描新增字节**——不得以文件大小整体失效（后台持续输出时每页整扫会随历史增长放大）；重启后首次访问重建。
- 剥离规则表驱动 Go 单测：颜色/OSC 标题/进度条 `\r`/CRLF 混合/二进制垃圾。

**完成证据**：分页遍历行号连续无缺漏无重复；持续追加的文件上连续翻页，扫描量不随文件整体增长；剥离单测边界全覆盖；活/死会话同一接口可读，死会话 404 仅限无记录。

## 单元 4：前端（uuid 寻址 + 历史视图）

**问题**：前端以 tmux 名为 id；没有历史浏览面。

**改动**：
- 全部请求与路由改用 uuid（列表字段、WS URL、close/rename 调用点）。
- 列表：活/死两态——死行置灰显示 `endedAt` 结束时间，行内删除图标（≥44×44，确认后 DELETE），整行点击进历史视图。
- 终端页头部「历史」入口 → 历史视图：只读纯文本行，上滚到顶加载更早页；**插入更早页时保持当前阅读位置不跳动；随加载深度窗口化裁剪远离视口的行，DOM 不随翻页深度无界增长**；返回终端页不断开后台会话。
- 移动端优先，同 Spec 001 约束（无 hover、触达达标、安全区）。

**完成证据**：`pnpm build` + vue-tsc 通过；DevTools 375 宽手测：列表 → 死会话历史 → 删除，终端 → 历史上滚翻页 → 返回接续。

## 单元 5：验收整合

**改动**：按 spec 验收 1–14 逐条自动化，沿用现有 Node 脚本模式（HTTP + WS + tmux/文件断言；现有脚本寻址从名字换 uuid 后重跑）。重点新增：改名 × 同名重建窗口的关闭正确性（`$N` 定位）、创建同队列不漏首条输出、split 后重武装仍绑原 pane、kill-server 后归档可读。前端交互项以 DevTools 移动仿真执行；真机体验留给用户。

**完成证据**：自动化全绿（含 -race）+ 供用户执行的真机清单。

## 风险与备注

- pipe-pane 的 `cat` 若因磁盘满/权限死亡，`pane_pipe` 归 0，靠列表查询时的重武装自愈；两次查询之间的静默丢失窗口可接受（个人工具）。
- `$N`/`%N` 仅在 tmux server 存活期内唯一，用户选项（`@limpet-id`/`@limpet-pane`）也存于 server 内存：所有内部 ID 定位都先核对 `@limpet-id`；kill-server 后会话与选项同亡，一切以注册表为准（spec 验收 14）。
- 注册表与 tmux 短暂不一致（kill-server 后首次列表才写 endedAt）——语义即「首次观测到死亡」。
- 寻址键迁移是一次性破坏（无外部消费者），前后端同批切换，无兼容层；现有测试脚本需同步改 uuid 寻址。
- 回放管线（capture-pane）本工作项不触碰，回归基线不变。
