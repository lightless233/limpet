# Plan 002：终端持久历史

- 日期：2026-11-23
- 对应 Spec：`docs/specs/002-2026-11-23-persistent-history.md`
- 执行方式：INLINE，单人单会话
- 依赖顺序：U1 → U2 → U3 → U4 → U5（U2 与 U3 可交错；U4 依赖 U1 的 API 形状）

## 单元 1：稳定 ID 与注册表

**问题**：会话身份 = tmux 名（`server/term.go` rename 时 map 重键 + entry.id 更新），改名即断接续；死会话无任何来源。

**改动**：
- 创建路径分配 uuid：`set-option @limpet-id` → 注册表登记（name/createdAt）。创建返回体、`GET /api/term` 列表、WS 路由、close、rename 全部以 uuid 寻址；rename 只更新 tmux 名与注册表 name。
- uuid → tmux 名解析：`list-sessions -F '#{session_name} #{@limpet-id} #{session_created}'` 扫描匹配，接入仍用精确目标 `-t =<名>`。
- 注册表 `~/.limpet/sessions.json`：原子写 + 互斥；结束判定（首次观测不在 tmux 中 → endedAt）；close 与 exit 同路径。
- 列表响应扩展（id/name/created/attached/live/hasHistory），活在前、死后依时倒序。
- `DELETE /api/term/{id}`：仅死会话，删条目 + 历史文件；活会话 409。

**完成证据**：rename 后同一 uuid 可接入且注册表 name 已更新；close/exit 后列表出现死条目；同名再建 uuid 不同；DELETE 语义（活 409 / 死删除）。

## 单元 2：pipe-pane 记录通道

**问题**：没有任何磁盘记录；断开与离线期间的输出无处可寻。

**改动**：
- 创建时启动 `pipe-pane -t <paneID> 'cat >> ~/.limpet/history/<uuid>.log'`（记录程序归属 tmux）；启动失败不阻断创建，`hasHistory=false` 等重武装补上。
- 重武装（硬约束：先查 `#{pane_pipe}`，为 0 才启动——3.3a 实测对活 pipe 二次 `pipe-pane -o` 会把它关掉）：Go 启动时 + 每次 `GET /api/term` 时对活会话检查。
- 存量迁移：无 `@limpet-id` 的活会话 → 分配 uuid → `capture-pane -p -e -S -50000 -E -1` 种子回填 → 启动 pipe。
- `tmuxBin()` 覆盖照常生效。

**完成证据**：浏览器断开后输出持续进文件；Go 重启（不杀 tmux）后记录继续；存量会话重启后被回填并可翻阅。

## 单元 3：历史读取 API

**问题**：磁盘上有字节流，没有读取路径。

**改动**：
- `GET /api/term/{id}/history?before&limit`，合同见 spec：LF 对齐行切分、ANSI 剥离（CSI/OSC/其他 ESC）、孤 `\r` 行内覆写、非法 UTF-8 → U+FFFD；行索引按需构建，内存缓存以文件大小失效。
- 剥离规则表驱动 Go 单测：颜色/OSC 标题/进度条 `\r`/CRLF 混合/二进制垃圾。

**完成证据**：分页遍历行号连续无缺漏无重复；剥离单测边界全覆盖；活/死会话同一接口可读，死会话 404 仅限无记录。

## 单元 4：前端（uuid 寻址 + 历史视图）

**问题**：前端以 tmux 名为 id；没有历史浏览面。

**改动**：
- 全部请求与路由改用 uuid（列表字段、WS URL、close/rename 调用点）。
- 列表：活/死两态——死行置灰显示结束时间，行内删除图标（≥44×44，确认后 DELETE），整行点击进历史视图。
- 终端页头部「历史」入口 → 历史视图：只读纯文本行，上滚到顶加载更早页，返回终端页不断开后台会话。
- 移动端优先，同 Spec 001 约束（无 hover、触达达标、安全区）。

**完成证据**：`pnpm build` + vue-tsc 通过；DevTools 375 宽手测：列表 → 死会话历史 → 删除，终端 → 历史上滚翻页 → 返回接续。

## 单元 5：验收整合

**改动**：按 spec 验收 1–12 逐条自动化，沿用现有 Node 脚本模式（HTTP + WS + tmux/文件断言；现有脚本寻址从名字换 uuid 后重跑）。前端交互项以 DevTools 移动仿真执行；真机体验留给用户。

**完成证据**：自动化全绿（含 -race）+ 供用户执行的真机清单。

## 风险与备注

- pipe-pane 的 `cat` 若因磁盘满/权限死亡，`pane_pipe` 归 0，靠列表查询时的重武装自愈；两次查询之间的静默丢失窗口可接受（个人工具）。
- 注册表与 tmux 短暂不一致（kill-server 后首次列表才写 endedAt）——语义即「首次观测到死亡」。
- 寻址键迁移是一次性破坏（无外部消费者），前后端同批切换，无兼容层；现有测试脚本需同步改 uuid 寻址。
- 回放管线（capture-pane）本工作项不触碰，回归基线不变。
