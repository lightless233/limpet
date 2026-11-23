# Spec 003：自研会话宿主（libghostty worker），退役 tmux

- 日期：2026-11-23
- 状态：待评审
- 来源：会话决策（2026-11-23）：PTY 持有进程自主维护，彻底抛弃 tmux 依赖。技术前提——libghostty-vt 1.0（2026-11-06，C ABI 稳定）、官方 go-libghostty 绑定、Terminal C API（网格/scrollback 读取）、snapshot encode/restore 示例
- 关联：取代 Spec 001 的「tmux 宿主」模型——001 的帧协议、启动顺序不变量、生命周期语义、移动端约束全部继承；Spec 002 暂缓（hold），其 uuid 身份由本工作项落地，历史 API/历史视图仍留 002 范围

## 背景与决策

001 以 tmux 为宿主。过去一个月的修复——capture 断线竞态、spawn 窗口、交接误杀、`%N` 撞名、前缀寻址、pipe-pane `-o` 怪癖、`@limpet-id`/`$N` TOCTOU——同属一类：**与外部进程的寻址和生命周期耦合**。tmux 对我们最有价值的是独立守护进程（崩溃隔离）与断开期间的网格维护；前者 worker 架构可以自建，后者 libghostty-vt 已提供且质量有背书（Neovim/JetBrains 采用）。名字即身份、无持久元数据则是 tmux 最弱的部分，逼出了整个 002 的对账层。

决策：**宿主自研（一会话一 worker 进程），tmux 完全退役**。会话与 Shell 的存活不再依赖任何外部组件，Limpet 自己就是宿主。

## 架构：一会话一 worker

```text
H5 <--- WSS（帧协议不变）---> limpet-server（路由/注册表/HTTP API/监督）
                                  |  unix socket，每会话一个
                      limpet worker <uuid>  ← server 重启后扫目录找回
                      ├─ 持有 PTY（shell 进程组，首次接入时启动）
                      ├─ libghostty-vt 网格状态（持续喂原始字节）
                      ├─ raw 录制 tee → ~/.limpet/history/<uuid>.log
                      └─ serve：重接快照/回放、输入、resize、exit 通知
```

- **worker** 是 PTY 持有进程：同一二进制的 `worker <uuid>` 子命令，独立生命周期。职责上限刻意收窄——单会话、单 pane、无复用、无名字解析。
- **server** 退化为路由 + 注册表 + 监督：spawn worker、扫描 socket 目录恢复列表、转发帧、提供 HTTP API。
- **前端零改动**（除会话 id 的值从 tmux 名换成 uuid）。
- 部署仍单文件（server 自 spawn worker）；libghostty 静态链接（cgo，仅依赖 libc）。

## 语义变更（需要评审确认）

**修订一：会话身份。** 001 的「会话 ID 即 tmux 会话名」废止（即 002 修订一）：创建时分配稳定 ID（uuid，8 字节 hex），作为唯一公开身份；改名后 ID 与接入地址不变，tmux 会话名降级为纯显示名。socket 文件名即 uuid。

**修订二：Shell 启动时机。** 001 的「创建即启动 Shell」修订为「创建即启动 worker；**PTY 在首次接入时、以首条有效尺寸启动**」。001 的启动顺序不变量（尺寸 → spawn → ready → 开放输入，提前输入 1008）保持不变，且**计时归属不变**：首条尺寸 / 3s 兜底 / query 兜底仍在 server WS 层（现有逻辑原样保留），worker 只接收「以 W×H 启动」的指令，不自行计时。副产品：从未接入的会话不产生 Shell 进程，「创建成功、接入失败遗留 Shell」一类问题连根消失。录制随 PTY 启动开始。

**修订三：进程隔离性质（新增）。** 001 验收 8 的行为不变（server 重启后会话全存活、接入如常）；新增隔离保证：**单个 worker 崩溃只死亡该会话**，其余会话与 server 不受影响（tmux kill-server 则全灭）。

**修订四：内置录制，历史功能不出场。** worker 从 PTY 启动起把输出原始字节 tee 到 `~/.limpet/history/<uuid>.log`——它既是回放的地基，也是 002 未来重启时的存储地基；但历史浏览 API/UI 不在本工作项范围（002 hold 中）。

## 帧协议（不变）

- 二进制帧 = 终端 I/O 原样转发；文本帧 = 控制 JSON（客户端→`resize`；服务端→`ready`）。
- 启动顺序不变量保持：Accept 后等首条有效尺寸 → worker 启动 PTY → ready → 开放输入；3s 兜底 + query 兜底；启动前二进制输入 1008。
- 关闭码语义保持：1000（会话结束）、1008（协议违规/未就绪）、1011（PTY/worker 启动失败）、409（单接入）、404（会话不存在）、426（非法握手）。**503 删除**——它的触发条件是「tmux 二进制缺失」，新架构无外部依赖，该分支永远打不中；worker spawn 失败属内部错误（500 + 原因）。

## Worker 契约

- **创建**：`POST /api/term` → server spawn worker（记录 uuid）→ worker 打开 `~/.limpet/workers/<uuid>.sock`（目录 0700）等待指令；此刻列表可见、Shell 未启动。
- **接入**：WS Accept → server 连接 worker socket（握手携带协议版本）→ server 把定案尺寸作为启动指令下发 → worker 启动 PTY：shell 取 `$SHELL`（缺省回落 passwd 条目）、交互模式、进程组隔离、起始目录 `$HOME`、`TERM` 由 worker 显式设置（不信任 server 环境）→ 回放（见下节）→ ready → 双向转发。
- **单消费者**：worker socket 同时只接受一个消费者，第二个接入报 busy，server 映射为 409（与 WS 层 claimed 双保险）。
- **输入**：字节透传，不缓冲（Esc 零延迟——今天 tmux escape-time 10 的等价物是「不做什么」）。
- **resize**：worker 对 PTY 执行 TIOCSWINSZ + SIGWINCH，同时通知自身网格重排；网格重排与 PTY 尺寸同源。
- **client 流过滤（有意识的保留，需评审确认）**：worker 发往浏览器的流中过滤 `smcup`/`rmcup`；**worker 自身网格吃原始流**（保持 alt-screen 真实状态，供快照使用）。说明：当年禁备用屏是因为 tmux 架构下「vim 进备用屏 = 浏览器滚动缓冲消失」无解；如今客户端是真 ghostty，该理由已消失，保留只剩两条理由——与已验证的行为平价（vim 期间浏览器仍可滚、退出后残迹留在滚动历史），以及快照/回放路径无主备屏歧义。标准终端行为（不过滤：vim 进真备用屏、退出还原画面、期间不可滚）是更简的替代方案，若要切换应作为独立小工作项交用户决定，**不在迁移中夹带**。
- **网格容量**：scrollback 与回放深度对齐（10000 行 + 余量）——worker 网格的唯一消费者是重接回放，更深的历史由 raw 录制承担（002 地基）。50000 是 tmux `history-limit` 的遗留数字，在新架构没有读取方，不随迁。resize 重排由 libghostty 承担。
- **录制**：PTY 启动即 tee 原始输出字节到 `~/.limpet/history/<uuid>.log`；磁盘故障只降级录制，不影响会话；不对外暴露任何历史 API。
- **退出路径**：
  - Shell 退出 → worker 通知当前消费者（1000 语义）、移除 socket、进程退出；
  - 显式关闭 → server 令 worker 终止 shell 进程组（TERM → 兜底 KILL）、移除 socket、退出；
  - worker 崩溃 → socket 遗留为 stale，由 server 扫描时检测清理，会话按首次观测死亡登记。
- **server 重启恢复**：启动时扫描 `~/.limpet/workers/`，socket 存在且进程存活 → 会话恢复；懒连接（首次接入时才握手）。

## 回放与画面重建（机制经 U0 spike 定案）

**保真合同（不降级）**：每次接入（首接与重接）恢复当前屏 + 最多约 10000 行 scrollback，含颜色；vim 等全屏应用界面原样恢复（001 验收 2/3 的保真不减）。

候选机制按优先级：
1. **快照 restore**：worker 用 libghostty snapshot encode 完整状态，客户端 WASM（@wterm/ghostty）restore——若客户端支持，重接带宽 O(当前屏 + scrollback 尾)，保真最高；
2. **网格序列化**：worker 从 libghostty 读网格/scrollback（需 cell 级含样式读取），自建「网格 → 转义序列」序列化器（capture-pane -e 的等价物）；
3. **raw 尾部回放**：不满足保真合同（alt-screen 退化），仅在 1、2 均不成立时作为过渡，须向用户明确降级并同意。

U0 spike 回答三问后，本节定案：① go-libghostty 绑定可用性；② 网格/scrollback 读取的实际 API 形状（cell 级样式是否可读）；③ 客户端快照 restore 是否可行。

## 会话身份与注册表

- uuid 在创建时由 server 分配；socket、历史文件均以 uuid 命名，天然杜绝同名冲突。
- 注册表 `~/.limpet/sessions.json`（server 单写者，原子写）：`{uuid → {name, createdAt, endedAt|null}}`。存活判定 = socket 存在且 worker 进程存活；死亡在首次观测时写入 `endedAt`（Shell 内 exit 与显式关闭同路径）。注册表是死亡会话的唯一记录。
- 列表 = worker 扫描 + 注册表字段合并，响应 `{id(uuid), name, created, attached}`（归档列表/endedAt 展示仍属 002 范围，不出场）。接入状态（attached）仍是 server 内存态：重启后一律视为未接入（001 语义不变）。
- 名称唯一性（409）与字符校验规则**作为产品选择保留**（防手滑建出同名任务）——uuid 世界里重名已无技术危害，tmux 的技术理由消失；放开重名、放宽字符集留待后续独立工作项（届时连同前端提示一起改）。

## 约束与边界

- **无兼容层**：现存 tmux 会话在切换时一次性失效（001 退役直接 PTY 时的先例重演）。
- 单二进制部署；`go build` 产出含 cgo（libghostty 静态链接），仅 WSL/Linux 本机目标。
- socket 目录权限 0700，单用户假设不变（001 非目标条款继续成立）。
- libghostty-vt 1.0 的 ABI 稳定，但 scrollback/reflow 等子 API 仍标注「可能演进」：版本 pin，升级走回归。
- 每会话一进程的内存量级（Go runtime + 网格 + 与回放深度对齐的 scrollback）低于 tmux 时代（50k 行）。
- **升级兼容（必须）**：server 升级 = 替换二进制 + 重启，运行中的 worker 仍是旧版本进程——socket 协议带版本握手，新 server 必须兼容运行中 worker 的协议版本（加法演进、不破坏性变更），保证升级不弃养运行中的会话。

## 非目标

- 历史浏览 API/UI、归档会话列表（002 hold 中的范围）。
- 多客户端接入、多 pane/split；**同时失去嵌套 tmux 逃生舱**（在会话里手动 split 不再可能）——单 pane 产品模型不变，此损失被接受。
- 历史搜索/导出、wss/TLS（开发期 localhost 约定不变）、Windows/macOS 宿主验证。
- 快照格式的跨版本兼容（worker 与 server 同二进制一起升级，socket 协议版本握手已覆盖运行中 worker 的兼容，见约束）。

## 验收标准

1. `POST /api/term`：列表立即出现（uuid 为 id、名字为显示名）；worker 进程与 socket 存在；**无 Shell 进程**（未接入不启动 PTY）。
2. 首次接入：首条尺寸决定 PTY 尺寸；ready 先于输出；提示符目录为主目录（`$HOME`）；ready 前二进制输入 1008。
3. vim 接续原样恢复（含 server 重启后重接的场景）。
4. 长输出后断开重连：直接向上滚动可见断开前历史行，含颜色。
5. Shell 内 exit：列表消失、接入 404、socket 清理、`endedAt` 登记。
6. 显式关闭：shell 进程组终止、列表移除、socket 清理。
7. 重命名：**id 与接入地址不变**；重名 409；非法字符 400。
8. 同会话第二个 WS 客户端 409；worker 层单消费者语义成立。
9. server 重启（`kill -9` 与**替换二进制的升级**两场景）：全部会话存活、运行中任务输出持续（录制不中断）、列表恢复（含名字）、接入画面如常，新旧版本 worker 与 server 协议握手成立。
10. worker `kill -9`：仅该会话死亡且列表如实，其余会话不受影响，stale socket 被清理，历史文件保留。
11. 帧级回归全绿：1.1MB 中文粘贴 md5 保真、64KiB 切块、1011 启动失败路径、426。
12. 回放保真按定案机制验收：快照 restore 路径——重接字节量 O(屏 + scrollback 尾)；序列化路径——与 capture 回放等价抽查比对。
13. 移动视口（DevTools 375 宽）全流程回归：新建 → 接入 → 返回 → 换会话 → 关闭，前端仅 id 值变化，无行为回归。
14. `go build` 单二进制 + `-race` 干净；tmux 依赖清零（代码与运行时均无 tmux 调用）。
