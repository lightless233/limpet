# Spec 006：本地 CLI 启动与多端接管（`limpet`）

- 日期：2026-11-26
- 状态：已吸收预审反馈（待开工）
- 来源：2026-11-26 会话讨论——用户希望直接在本地终端运行 `limpet codex --yolo` 等 TUI 并自动纳入 Limpet 管理，本地保留原生贴图与快捷键体验，随时在手机或浏览器查看与接管
- 关联：延续 Spec 003（worker 独立宿主）、Spec 004（多视图会话与尺寸跟随输入者）、Spec 005（systemd 部署）；**本工作项前端零改动**

## 目标

- 在本地终端直接运行 `limpet <cmd> [args...]` 或 `limpet`，按当前工作目录与完整本地环境启动目标程序并纳入 Limpet 管理，手机 / 浏览器端无需额外操作即可看到并接入同一会话。
- 保留本地原生终端体验：原始字节与控制序列直通（含 WSL 下 `Ctrl+V` 读系统剪贴板贴图、拖拽路径、快捷键、OSC 52 等）。
- 提供 `limpet ls` 与 `limpet attach <name|id>`，支持随时查看活跃会话或重新接回后台继续运行的会话。
- 单二进制承载全部角色（保留 `limpet-server`，部署时补充软链 `~/.local/bin/limpet`），不破坏既有 Web 创建、查看、关闭与重命名路径。

## 一、CLI 入口与命令契约

### 1.1 调用名与命令分派

根据 `filepath.Base(os.Args[0])` 与命令行参数分派职责：

- **任意调用名下保持既有子命令**：
  - `version`：打印构建版本号并退出。
  - `worker <uuid>`：以 worker 模式运行指定 UUID 会话。
- **非 `limpet` 调用名且无上述子命令时**：直接启动 HTTP server（完全兼容 `limpet-server` 及测试构建的二进制文件名）。
- **`limpet` 调用名下支持以下入口**：
  - `limpet`（无参数）：在当前工作目录启动默认交互式 Shell（优先 `$SHELL -i`，回退 `/etc/passwd` 登录 Shell，再回退 `/bin/bash`）并接入。
  - `limpet [-n|--name <name>] [--] <cmd> [args...]`：启动目标命令 `<cmd> [args...]` 并接入。
    - 顶层标志（`-n` / `--name` / `-h` / `--help`）**仅在命令名或 `--` 之前解析**。
    - 一旦遇到首个非标志参数（即 `<cmd>`）或 `--`，立即停止顶层参数解析，后续全部参数原样透传给 `<cmd>`（例如 `limpet codex --yolo -m o3` 必须将 `--yolo -m o3` 原样传给 `codex`）。
    - 目标命令名与内置子命令（`ls`、`attach`、`server`、`version`、`worker`、`help`）冲突时，可通过 `limpet -- <cmd> ...` 显式消歧。
  - `limpet ls`：列出当前 `$LIMPET_HOME` 下全部活跃会话，输出包含名称、16 位 hex ID、当前接入状态（`attached`）以及可用时的工作目录/命令展示名；优先查询本机 `server.sock`，server 未运行时回退扫描 `$LIMPET_HOME/workers/*.sock` 并结合注册表展示最终名称。
  - `limpet attach <name|id>`：按精确会话名或 16 位 hex ID 重新接入正在运行的会话（优先查本机 `server.sock`；server 未运行时只读合并 `$LIMPET_HOME/sessions.json` 与 `workers/*.sock` 探测结果，优先使用注册表里的最终名称/重命名结果，否则使用 worker 建议名称或 UUID）。
    - 未找到目标时输出明确中文错误并以非零退出码结束。
    - 若离线或名称与其它会话 UUID 发生歧义（匹配超过 1 个活跃会话），输出明确歧义提示及候选 ID 列表并以非零退出码结束。
  - `limpet server`：显式以 HTTP server 模式启动。
  - `limpet help` / `-h` / `--help`：输出简明中文帮助信息并正常退出。

### 1.2 前提校验与目录隔离

- **TTY 前提**：`limpet`、`limpet <cmd> ...` 与 `limpet attach ...` 要求标准输入与标准输出均为交互式终端（TTY）。任一端非 TTY 时，直接向标准错误输出清晰中文原因并以非零退出码返回，**不得**切换终端模式，也不得拉起 worker。
- **数据目录隔离**：所有 CLI 子命令与本地控制通道均严格遵守 `LIMPET_HOME`（未设置时默认 `~/.limpet`；开发与测试通过 `LIMPET_HOME=~/.limpet-dev` 隔离）。

## 二、Worker 启动配置、首 Viewer 门闩与环境契约

### 2.1 向后兼容的 Web 创建路径

经 HTTP `POST /api/term` 由 server 拉起的 `worker <uuid>`（无额外 CLI 启动配置）行为与现有 Spec 003/004/005 保持完全一致：`$SHELL -i`、工作目录 `$HOME`、初始尺寸 `80×24`、`TERM=xterm-256color`、worker 启动后立即拉起 Shell，且仍走 `launchDirect` / `systemd-run`。

### 2.2 CLI 启动配置、独立会话与环境继承

CLI 拉起 worker 时传递启动配置：`argv`（完整命令及参数）、`cwd`（调用方当前工作目录）、初始终端尺寸 `cols×rows`、建议会话名 `name`，以及 `waitFirstViewer` 标志。

- **传递与脱敏边界**：启动配置不得通过 `/proc/<pid>/cmdline` 明文命令行参数暴露，也不得在磁盘遗留配置临时文件（通过继承匿名管道文件描述符传递并在读取后立即关闭）。**完整 `argv` 仅用于启动目标进程本身，严禁通过 `probe`、控制列表或 `sessions.json` 持久化/对外返回**；展示及元数据字段 `cmd` 仅保留可执行文件基名（`filepath.Base(argv[0])`，如 `codex`、`zsh`）。
- **独立会话边界**：CLI 以 `Setsid` 独立会话方式拉起 worker（使其与本地终端会话及 `limpet-server.service` 控制组均相互独立，关闭本地终端标签页 `SIGHUP` 或重启 `limpet-server` 均不影响其存活）；同时明示其仍位于启动它的终端用户控制组内（这与 Web `systemd-run --user` 独立临时单元处于不同控制组边界）。
- **环境继承**：子进程直接继承调用方（即 worker 继承自本地 CLI）的完整环境变量（确保 `WSL_INTEROP`、`DISPLAY`、`WAYLAND_DISPLAY`、`PATH`、代理设置、虚拟环境、认证环境等与直接本地运行完全一致），仅剔除内部 `LIMPET_*` 配置变量（但保留 `LIMPET_HOME`）。若调用方已有非空 `TERM`，原样保留；仅在 `TERM` 为空时回退 `xterm-256color`。

### 2.3 首 Viewer 门闩（启动权认领与无竞态交接）

在 `waitFirstViewer` 模式下，为彻底消除「目标程序启动瞬间按 `80×24` 排版闪屏」及「`libghostty` 提前代答首个终端能力查询」的竞态，建立明确启动权交接契约：

1. **只暴露未就绪 socket**：worker 先创建并监听 `$LIMPET_HOME/workers/<uuid>.sock`（`0600`），此时尚未初始化会话，也未启动子进程与 PTY 读循环。
2. **专用启动握手（`op: "start"`）**：启动方 CLI **先进入 raw mode**（任何失败都保证退出时恢复），再通过专用启动操作 `start` 认领门闩。普通 `attach` 在会话进入运行态之前必须被拒绝，不能抢开门闩。
3. **先登记首 viewer 切点再起进程**：认领成功后，worker 在启动目标命令与 PTY 读循环**之前**，按配置里的真实初始 `cols×rows` 初始化双网格与录制（录制首条 `S` 尺寸事件必须记录真实 `cols×rows`），并立即将首 viewer 登记入 `viewers` 且实时流切点设为 `0`；随后启动目标命令并开启 PTY 读循环。
4. **固定首帧顺序**：首 viewer 不走 formatter 画面重建，固定帧序为 `handshakeAck(ok=true) → ftReady → 原始 PTY 输出流`（与从 0 开始的实时流一致），保证目标程序起手发出的终端能力查询直接透传给真实本地终端应答，不被 `libghostty` 提前拦截代答。
5. **单一状态转移与边界处理**：
   - `start` 认领、门闩超时（默认 10 秒）、远程 `kill` 与 worker 自身 `SIGTERM` 由同一状态转移协调：取消路径先赢则禁止启动子进程、关闭并清理 socket；启动先赢则转入正常会话生命周期统一收尾。
   - 等待启动与启动中期间收到 `probe` 时，应答就绪标志为 `false`（`Ready: false`）等状态字段，server 不得提前纳管该会话，也不得误判为失效 socket 删除。
   - **启动失败无残留**：若目标命令启动失败（如命令不存在或无执行权限），worker 有界向该 `start` 连接发送 `handshakeAck(ok=false, reason=...)` 后，删除本次新建的 `.log`/`.meta` 录制文件与 `.sock`，不留空录制、半死会话或 `sessions.json` 垃圾记录。

### 2.4 退出码、进程树回收与尾部输出顺序

- **单一主进程收割者**：主进程只能有唯一 waiter 收集其真实 `WaitStatus`（禁止并行 `Wait4(-1)` 抢夺主进程等待状态），主进程退出事件驱动后续收尾；在收割过继孤儿后代前，先等待主进程结束并取得 `exitCode`（正常退出取 `0..255`；被信号终止取 `128 + signal`）。
- **防止后代持有 PTY 导致卡死**：主进程退出后，读循环尽量排空 PTY 剩余缓冲；若仍有孤儿后代继续持有 slave PTY 导致短宽限后仍无 EOF，必须触发后代进程树回收以关闭 PTY 写入端并排空退出，避免 worker 无限挂起。
- **区分退出原因与真实 `WaitStatus`**：保留明确关闭原因（如 `shell-exit` / `command-exit`、远程 `kill`、worker 故障），不与进程是否被信号终止混淆。
- **尾部输出先于 `ftExit` 到达**：对于正常消费的 viewer，停止 PTY 读取并等待未发送队列通过同一输出泵写完之后，再发送 `ftExit`（确保 `printf done && exit 0` 这类短命令在 `ack → ready` 之后不丢尾部输出）；慢消费者仍受现有背压队列上限与有界写超时约束，不被无限阻塞。

## 三、Worker 协议增量扩展与活跃尺寸所有者

### 3.1 帧载荷兼容性基线

以当前仓库实现为基线（帧格式 `[type:1][len:4 BE][payload]`，已无 `proto` 版本字段）：

- `handshakeReq.Op` 新增 `"start"`（专供 CLI 首 viewer 认领门闩）。
- `handshakeAck` 新增可选字段（`omitempty`）：`name`、`cwd`、`cmd`（仅可执行文件基名）、`createdAt`（Unix 毫秒）、`ready`（`bool`，配合现有 `OK` 区分等待中与可纳管运行态；旧 worker 缺失或运行中时按正常就绪理解）。
- `exitMsg` 新增可选字段（`omitempty`）：`exitCode`（`*int`）；不重复增加 `error` 字段（启动失败沿用 `handshakeAck.Reason`）。对于不带 `exitCode` 的现存旧 worker，CLI 收到 `ftExit` 时明确按未知状态处理并恢复终端（以非零兜底码如 `1` 或按退出原因退出，不将 `nil` 误当作 `0`）。
- Web 侧保留现有 `ftExit → WS 1000`、`ftDetach → WS 4001` 与独立心跳机制。

### 3.2 活跃尺寸所有者与 `SIGWINCH` 即时响应（对 Spec 004 的最小补充）

- **限定范围**：**仅针对 CLI 创建的会话**启用活跃尺寸所有者即时 `SIGWINCH` 生效；纯 Web 创建的会话行为与 Spec 004 保持 100% 一致（只有发送 `ftInput` 时才触发尺寸晋升）。
- **串行化规则**：在 CLI 会话内，首个通过 `start` 接入的本地 viewer 为初始活跃尺寸所有者；之后任一 viewer 发送有效 `ftInput` 时，在串行输入锁保护下切换活跃所有者为该发送者；活跃所有者断开时清空所有者但不主动回退网格尺寸。
- **Resize 处理**：当某 viewer 发送合法 `ftResize` 申报新尺寸时：
  - 若其是当前活跃所有者且新尺寸不同于网格当前尺寸，立即同步调整 PTY 与双网格尺寸（无需等下次按键）。
  - 若其不是活跃所有者（例如手机旁观者），仅保存其申报尺寸，不改动网格与 PTY。
- **`limpet attach` 首尺寸声明**：CLI 重新 `attach` 到已有会话时，接入后先发送一条当前本地终端尺寸的 `ftResize` 申报帧，再转发后续键盘输入，保证首次打字即可按本地窗口尺寸晋升（符合 Spec 004 已知行为：若多端存在终端自动查询回包，回包经 `ftInput` 同样会参与尺寸晋升）。

## 四、Server 本机控制通道与动态纳管

### 4.1 目录独占锁与 `server.sock` 安全边界

- **进程级目录独占**：`limpet-server` 启动流程**必须在读取 `sessions.json`、恢复 worker、清理残留 socket 之前**，先获取 `$LIMPET_HOME/server.lock` 的非阻塞排他文件锁（`flock`）并持有至进程退出；获取失败则直接报错退出，杜绝双 server 并发篡改注册表或互删监听 socket。
- **控制 Socket**：取得锁后清理已失效残留（若有），监听 `$LIMPET_HOME/server.sock`（权限 `0600`），优雅退出时关闭并移除；对每条控制连接设置明确消息体上限（如 16 KiB）与读写超时（如 2 秒）。
- **对外 HTTP 面不变**：`/api/term` 返回字段集与既有 Web 路由保持完全不变，不增加暴露额外敏感字段或执行命令的 HTTP 接口。

### 4.2 本机操作契约

控制通道使用行/帧分隔 JSON 请求与应答，提供两个本机操作：

- **`register`**：接收 `{uuid, name, explicitName}`（仅命名意图，`cwd`、`cmd`、`createdAt` 由 server 经核验 `probeWorker` 从 worker 获取，防止客户端伪造或遗留完整 `argv`）。
  - 确认对应 worker 已就绪（`Ready` 且 UUID 匹配）后，在单写者互斥保护下分配最终名称、纳入内存会话表、持久化 `sessions.json`，并返回最终登记名。
  - `regEntry`（`sessions.json`）新增可选字段 `cwd`、`cmd`（`omitempty`），与旧版 `sessions.json` 兼容。
- **`list`**：返回活跃会话完整控制视图（含 `id`、`name`、`created`、`attached`、`cwd`、`cmd`），供 `limpet ls` 与 `limpet attach` 使用。

### 4.3 命名、防复活与重名规则

- **默认命名**（未传 `-n`/`--name`）：CLI 按 `<当前目录末段>-<命令基名>` 生成建议名称并清洗（将 `:`、`.` 及控制字符替换为 `-` 或移除，按 UTF-8 rune 截断，留足后缀余量并在最终入表前保证满足 `nameValid`：非空、≤100 字符）；清洗为空时回退 `uuid`。
- **显式命名**（传入 `-n`/`--name`）：
  - CLI 先在本地执行 `nameValid` 校验，非法立即报错退出。
  - 若 `server.sock` 可达且通过 `list` 查得同名活跃会话，CLI 在拉起 worker 前快速失败报错（已知竞态说明：不做分布式 `reserve` 租约状态机，极小概率并发撞名或离线后撞名统一由服务端追加 `-2`、`-3`……保证不丢失活跃会话且不误杀已运行程序，追加后缀后总长度仍必须满足 `nameValid` ≤ 100 rune 上限）。
- **统一入表与防复活不变量**：启动恢复（`recoverWorkers`）、动态扫描与 `register` 共用同一入表规则——`probeWorker` I/O 在锁外执行，落盘回写前持 `m.mu` 复核：
  1. 跳过本 server 正在拉起中的 `pendingIDs`（及未就绪 `Ready=false` 的 worker）；
  2. 若 `registry[uuid].EndedAt != nil`（说明该会话已被显式关闭并登记结束），**绝不再复活进活跃表**；
  3. 对已登记过名称的活会话，保留首次确认或已被用户 `rename` 的名称，重复 `register`/扫描幂等不覆盖；
  4. 按合法 UUID 单查未命中时，直接探测该单个 `<uuid>.sock` 即可，无需全目录扫描。

### 4.4 离线容错与降级边界

- **CLI 不强依赖 server**：CLI 拉起 worker 并开始会话后，向 `server.sock` 发送 `register`（短超时）；无论 server 当前未运行（`ENOENT`/`ECONNREFUSED`）还是瞬时超时，CLI 均照常继续运行当前会话（未即时登记成功的会话将由 server 后续动态扫描按同一入表规则纳管，避免额外重试状态机）。

## 五、本地 TTY 透传、断开与终端复位契约

- **进入与信号安装顺序**：保存原始 `termios` 并注册 `SIGHUP`、`SIGTERM`、`SIGWINCH` 处理后再进入 raw mode（确保信号抵达时不会遗漏恢复）。
- **单一可中断退出路径**：透传使用独立读/写协调结构——退出信号、socket EOF、`ftExit` 或写失败触发统一退出流程：先取消上下文、关闭 viewer socket、停止 stdout 写者，**不因空闲阻塞中的 `stdin.Read` 或卡死的 `stdout` 写而无限挂死**。
- **强制复位与正常退出区分**：
  - **目标程序正常退出**（收到 `ftExit` 且 `exitCode` 正常退出 `0..127`）：目标 TUI 通常已自行复位终端，CLI 直接恢复原 `termios` 并透传退出码，避免覆盖正常输出。
  - **需要强制发送终端复位序列的路径**：远程关闭触发退出、慢消费者 `4001` 丢弃、worker 连接异常断开（含缺失 `exitCode` 的意外结束）、CLI 收到 `SIGHUP`/`SIGTERM` 脱离，以及**目标进程被信号终止（`exitCode >= 128`）**（因被杀的 TUI 来不及自清理备用屏和输入模式）。
- **Kitty 双栈与安全复位序列**：复位序列在发送退出备用屏（`\x1b[?1049l`）**之前和之后各发一次 `\x1b[=0u`**（同时复位备用屏与主屏各自的 Kitty 键盘增强协议栈，不依赖未知栈深度 pop），并配合关闭鼠标追踪（`1000/1002/1003/1006`）、关闭 bracketed paste（`2004l`）与焦点事件（`1004l`）、显示光标（`?25h`）、重置 SGR（`0m`）。
- **可用 TTY 与已挂断 TTY 的区分**：当本地终端描述符仍可写可控时，保证有界写出复位序列并恢复原 `termios`；当终端窗口已物理关闭/挂断（`EIO`/`EPIPE`/`ENOTTY`）时，尽力尝试并忽略不可写错误、不阻塞收尾，且**绝不向 worker 发送终止信号**，确保后台会话与手机端持续存活。

## 六、部署与构建集成

- `deploy/install.sh` 在原子更新 `$install_dir/limpet-server` 后：
  - 先将 `$install_dir` 解析为绝对路径；
  - 若 `~/.local/bin/limpet` 已存在且**不是符号链接**（例如用户的普通文件或已有其它脚本），拒绝覆盖并给出明确中文提示（仅首次创建或更新既有软链至 `$abs_install_dir/limpet-server`）；
  - 若 `~/.local/bin` 不在 `$PATH`，打印一行提醒。
- `deploy/README.md` 同步补充 `limpet` CLI 入口说明，并在卸载步骤补充 `rm -f ~/.local/bin/limpet`。
- 不新增第三方 Go 依赖，不改动既有 `limpet-server.service` 单元定义与正式数据目录 `~/.limpet`。

## 七、非目标

以下工作明确不在 Spec 006 范围内：

- 任何前端 UI、页面列表或移动端交互改版。
- 手机端「保持原宽度不触发 resize / 缩放查看」模式。
- 已结束会话的历史回放页面及 `codex resume` 一键恢复界面（本工作项仅记录元数据与既有录制文件）。
- 本地手动 detach 快捷键（如 `--detach-key`）。
- 通过 HTTP API 接收任意执行命令或客户端环境变量。
- 分布式名字预占租约（`reserve`）或客户端注册重连/对账状态机。

## 八、验收标准

1. **CLI 分派与参数透传**：使用名为 `limpet` 的真实二进制运行 `limpet <cmd> [args...]`（含 `--yolo`、`--help`、`--version` 及 `-- ls` 消歧），参数 100% 原样透传；目标以 `0`、`42` 及信号终止退出时，CLI 退出码精确匹配（且被信号终止时额外触发终端复位）。无参 `limpet` 在当前工作目录拉起交互式 Shell。非 TTY 环境下拒启并立即报错，不修改终端状态也不残留 worker。
2. **启动无竞态与短命令尾部保序**：在 `140×40` PTY 中启动程序时，首屏按 `140×40` 呈现，终端能力查询直达本地终端应答（无 `libghostty` 提前代答），录制首帧 `S` 为 `140x40`；门闩超时无连接、`probe`/`kill` 并发或命令不存在失败时均有清晰状态且不留 `.sock`/空 `.log`/`.meta`；执行“立即输出后立刻退出”的短命令（含主进程已退但后代短暂占用 slave PTY 场景）尾部输出完整到达且按真实状态退出。
3. **活跃尺寸隔离**：CLI 会话内活跃端 `SIGWINCH` 立即调整 PTY/网格；纯 Web 会话与旁观者 `ftResize` 严格保持 Spec 004 行为不变；`limpet attach` 接入后先声明本地尺寸并在首次输入时晋升。
4. **控制通道、单实例锁与并发安全**：同一 `$LIMPET_HOME` 启动第二个 server 因 `server.lock` 立即失败且不破坏已有 `sessions.json` 或 `server.sock`；残留失效 `server.sock` 可自动恢复；`register`/`list`/外部扫描与并发 `create`/`close`/`rename`（含“`probe` 成功与 `close` 交错”）在 `-race` 下不重名、不复活已关闭会话、不丢失既有名称；`argv` 参数（如模拟 token）绝不出现在 `probe`、`list` 或 `sessions.json` 中。
5. **命名与离线 `ls`/`attach`**：`limpet -n` 非法名或在线重名在启动目标进程前拒绝；默认名与离线冲突自动加后缀并满足 ≤100 rune 上限；`limpet ls` 与 `limpet attach` 在 server 在线与离线两种状态下均可按最新名称（含 `rename` 后名称）或 ID 接入，同名歧义明确报错。
6. **TTY 恢复、复位与 `SIGHUP` 保活**：运行中发生 `SIGHUP`（模拟关窗）/ `SIGTERM` 脱离时，CLI 退出且不卡死在阻塞读写上，原 worker 与网页端保持存活；远程关闭正处于备用屏、隐藏光标、鼠标追踪、Kitty 键盘模式下的会话时，CLI 恢复原 `termios` 且输出双栈安全复位序列。
7. **部署脚本安全**：临时 `HOME` 下测试 `deploy/install.sh`（或软链逻辑）可创建绝对路径软链、重复执行幂等、遇到已有普通文件 `~/.local/bin/limpet` 拒绝覆盖；不触碰正式 `/opt/limpet` 与 `~/.limpet`。
8. **全套回归与边界测试**：`PKG_CONFIG_PATH=... go test -race -count=1 ./server` 全部通过；在 `LIMPET_HOME=~/.limpet-dev` 下复跑现有协议、多视图、备用屏重连、慢消费者、退出回收与浏览器重连测试（单测同步覆盖缺少可选字段的旧帧/旧 `sessions.json` 兼容）；清理时仅清理本轮测试 UUID。
9. **开发态真机手测**（单独标注、不与自动化混淆）：在 `LIMPET_HOME=~/.limpet-dev` 下本机真实终端运行 `limpet codex`，确认 `Ctrl+V` 贴图、原生快捷键、网页并发观看与接管、关 tab 后 `limpet attach` 接回与正常退出回收体验完备。
