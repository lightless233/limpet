# Plan 006：本地 CLI 启动与多端接管（`limpet`）

- 日期：2026-11-26
- 对应 Spec：`docs/specs/006-2026-11-26-local-cli-attach.md`
- 执行方式：DELEGATED（已完成执行前预审并修订定稿，待用户确认后由 GPT 按 U1 → U5 实施）
- 工程原则与防过度设计边界：
  - 不新增 `go.mod` 外部依赖（Linux raw mode 使用标准库 `syscall` termios + 已有 `github.com/creack/pty`），**前端零改动**。
  - **禁止过度设计**：不做分布式 `reserve` 租约状态机、不做客户端 `register` 后台重试/对账循环、不在生产代码里写测试专用同步钩子或故障注入框架、不回退历史 git commit 去编旧二进制（缺失可选 JSON 字段的兼容性用单元测试直接构造验证）。
  - 所有开发与测试均带 `LIMPET_HOME=~/.limpet-dev`；未获用户明确指示严禁运行 `deploy/install.sh` 上线或触碰正式 `~/.limpet`。

## 总体顺序与依赖关系

```text
U1（Worker start 门闩 / 单 waiter 退出码 / 泵内 ftExit / CLI 会话活跃尺寸）
  ├──▶ U2（flock 单实例锁 / server.sock / 统一防复活纳管）
  │      └──▶ U3（CLI 入口 / 离线合并 ls&attach / TTY 透传与安全复位）
  │             └──▶ U5（全量 Go + Dev 环境回归，清理只限测试 UUID）
  └── U4（deploy/install.sh 安全绝对路径软链 + deploy/README.md 卸载说明）
```

## 实施单元

### U1：Worker 参数化启动、`start` 门闩、单 Waiter 收尾与活跃尺寸

- **目标**：实现与现有 Web `worker <uuid>` 完全隔离的参数化启动、无竞态首帧输出、精确退出码捕获与按序 `ftExit` 投递，并将即时 `SIGWINCH` 尺寸调整严格限定在 CLI 会话的活跃所有者上。
- **涉及文件**：`server/proto.go`、`server/session.go`、`server/recording.go`、`server/workerapp.go`、`server/launch.go`、`server/main.go`。
- **关键改动与边界**：
  1. 启动配置通过继承匿名管道（`ExtraFiles`）传入 CLI worker 并立即关闭管道；仅把可执行基名作为 `cmd` 元数据，完整 `argv` 绝不写入录制 meta、控制接口、`probe` 或 `sessions.json`。复用 `launch.go` 的进程与环境过滤纪律（CLI 保留调用方完整 env 与非空 `TERM`，仅剔除内部 `LIMPET_*` 并保留 `LIMPET_HOME`）。
  2. 扩展 `handshakeReq` 支持 `op: "start"`，扩展 `handshakeAck` 可选字段（`name`、`cwd`、`cmd`、`createdAt`、`ready`），扩展 `exitMsg` 可选字段（`exitCode *int`）。
  3. 首 viewer 门闩与单一状态转移：
     - `waitFirstViewer` 下先监听 `workers/<uuid>.sock`，收到 `start` 之前拒绝普通 `attach`；`probe` 返回 `Ready: false`；`start` / `kill` / 门闩超时（10s） / `SIGTERM` 经统一状态保护仲裁。
     - 认领后在启动命令与 PTY 读循环**之前**，以真实 `cols×rows` 初始化双网格、`recording.write`（修复首条 `S` 事件尺寸）并将首 viewer 切点设为 `0` 加入 `viewers`，按 `ack → ready → 原始输出` 交付，跳过首 viewer 的 formatter 重建。
     - 若启动命令失败，有界返回 `OK=false` 原因后清理当次创建的 `.log`/`.meta`/`.sock`。
  4. 单一 Waiter 与泵内 `ftExit`：
     - 指定唯一收割者收集主进程 `WaitStatus` 并映射 POSIX 退出码（禁止与 `Wait4(-1)` 并发抢同一 PID），由主进程退出驱动收尾；若后代仍占用 slave PTY 超过短宽限，触发 `reapTree` 收回以确保读循环排空退出。
     - 关闭原因与 `WaitStatus` 解耦；对健康 viewer 通过同一输出泵排空已入队尾部字节后再写 `ftExit`，避免短命令截断。
  5. 活跃尺寸所有者（仅限 CLI 启动会话）：初始归 `start` viewer，`ftInput` 切换所有者，所有者断开清空；仅活跃所有者发来变化后的合法 `ftResize` 立即调整 PTY 与双网格，Web 创建的会话 100% 保留 Spec 004 原行为。
- **完成证据**：新增 Go 单测/集成测试覆盖无 `start` 超时、`probe`/`kill` 并发安全、启动失败无空 `.log`/`.meta` 残留、首输出直达不经 `libghostty` 代答（可用真实终端查询序列验证）、录制首条 `S` 准确、立即打印退出短命令、主进程退出但后代占用 PTY、退出码 `0`/`42`/信号退出、以及 CLI 活跃尺寸 vs Web 原样行为，`-race` 全绿。

### U2：目录独占锁、`server.sock` 与统一动态纳管

- **目标**：保证同一 `LIMPET_HOME` 仅有一个 server 读写注册表与控制 socket，消除外部注册、动态扫描与 `create`/`close`/`rename` 之间的并发竞态与已关闭会话复活漏洞。
- **涉及文件**：`server/manager.go`、`server/main.go` 及本机控制处理所在文件（同属 `server/` 包）。
- **关键改动与边界**：
  1. `main` 在调用 `newManager()` 之前获取 `$LIMPET_HOME/server.lock` 非阻塞排他 `flock` 并持有至退出；取得锁后再读 `sessions.json`、清理失效 `server.sock` 并监听 `server.sock`（`0600`，优雅退出移除）。控制请求强制载荷上限与读写超时。
  2. 实现 `register`（仅接收 `{uuid, name, explicitName}`，核验 worker `Ready` 且从 `probeWorker` 取 `cwd`/`cmd`/`createdAt`）与 `list`（输出含 `cwd`/`cmd` 的控制视图）；HTTP `GET /api/term` 返回结构保持零改动。
  3. 统一入表与防复活：
     - `manager` 按 UUID 追踪本 server 自建在途会话（`pendingIDs`）。
     - 启动恢复、动态扫描与 `register` 锁外 `probeWorker` 后，在 `m.mu` 内统一检查：非 `pendingID`、worker 已就绪、`registry[uuid].EndedAt == nil`（绝不复活已关闭会话）；已登记名称或用户 `rename` 结果不被重复 `register`/扫描覆盖；重名自动追加 `-2`、`-3`……时保证 UTF-8 rune 总长 ≤ 100。
     - `get(id)` 未命中且 `id` 合法时，仅探测单个 `<uuid>.sock`。
- **完成证据**：Go 测试（`-race`）覆盖双 server 同 `LIMPET_HOME` 互斥锁与失效 `server.sock` 恢复、未就绪 `Ready=false` worker 不被提前纳管、`register` 幂等与不覆盖 `rename`、已关闭会话（`EndedAt != nil`）不被扫描复活、超长重名截断后缀仍符合 `nameValid`、旧版 `sessions.json` 读取与缺失可选字段解码、以及并发 `create`/`close`/`rename`/外部发现的压力测试。

### U3：本地 CLI 入口、`limpet ls`/`attach` 与 TTY 透传复位

- **目标**：提供完整易用的 `limpet` CLI 命令分派、在线/离线 `ls` 与 `attach`、以及任何断开场景下都安全可靠且不误杀 worker 的终端透传与复位。
- **涉及文件**：`server/main.go`、CLI 与 Linux termios 终端工具文件（同属 `server/` 包）。
- **关键改动与边界**：
  1. 按 `filepath.Base(os.Args[0])` 分派，顶层参数解析严格停在首个命令名或 `--` 之后；严格检查 stdin/stdout 均为 TTY。
  2. 默认名清洗保证符合 `nameValid` 且 ≤ 100 rune；指定 `-n` 且 `server.sock` 可达时先查重名快速失败；拉起 worker 后发送 `register`（无论 server 离线或短超时均静默降级不阻塞会话）。
  3. `limpet ls` 与 `limpet attach` 在 `server.sock` 不可达时，只读合并 `sessions.json` 与活跃 `workers/*.sock` 探测结果以保留最终/重命名名称，同名冲突报明确歧义（列出候选 ID）。
  4. 透传与收尾：
     - 保存原 `termios` 并安装 `SIGHUP`/`SIGTERM`/`SIGWINCH` 后再进 raw mode（`start` 模式先 raw 后 `start`）。
     - `limpet attach` 重放重建画面前先发 `\x1b[2J\x1b[H` 与本地当前 `ftResize` 申报；新起命令不清屏。
     - 单一退出路径先取消上下文、关闭 socket、停止 stdout 写者（不被空闲阻塞 `stdin.Read` 或阻塞写卡死），随后区分正常 `Exited()` 退出与异常/远程关闭/`4001`/`SIGHUP`/`SIGTERM`/信号被杀（`exitCode >= 128`）：需要复位时在 `\x1b[?1049l` 前后各发 `\x1b[=0u` 及鼠标/paste/focus/光标/SGR 复位，并尽力恢复原 `termios`（TTY 已挂断时忽略错误不阻塞，绝不杀 worker）。
- **完成证据**：基于真实 PTY 对模拟本地终端的 Go 集成测试，覆盖 `limpet <cmd> --flag`、`--` 消歧、无参 Shell、`cwd`/`env`/`TERM` 继承、`limpet ls` 与 `rename` 后离线 `attach`、非 TTY 拒启、`SIGHUP` 运行中/启动中脱离保活、远程关闭与信号退出时 `termios` 还原及复位序列输出、缺 `exitCode` 旧帧处理、以及本地 CLI + WebSocket 双视图实时互通。

### U4：部署软链与文档最小同步

- **目标**：部署脚本安全创建绝对路径 `~/.local/bin/limpet` 软链并更新文档。
- **涉及文件**：`deploy/install.sh`、`deploy/README.md`。
- **关键改动与边界**：
  1. `deploy/install.sh` 将 `$install_dir` 转为绝对路径后再为 `~/.local/bin/limpet` 建软链；若目标路径已存在且非符号链接，拒绝覆盖并报中文错误；若 `~/.local/bin` 不在 `PATH` 则提示。
  2. `deploy/README.md` 补充 `limpet` 入口简介与卸载时的 `rm -f ~/.local/bin/limpet`。严禁执行 `deploy/install.sh` 或触碰 `/opt/limpet` 与 `~/.limpet`。
- **完成证据**：`sh -n deploy/install.sh` 通过；在临时目录 + 临时 `HOME` 中验证首次建软链、重复幂等、绝对路径正确性以及非软链文件拒覆盖（不触发真实服务安装）。

### U5：全量开发态验证与真机待验清单

- **目标**：在 `LIMPET_HOME=~/.limpet-dev` 下收口全部 Go 与 JS 回归验证，保持开发数据目录干净，明确区分自动化已证与真机待测项。
- **验证要求**：
  1. 运行 `PKG_CONFIG_PATH="$HOME/.local/opt/limpet-toolchain/install-1.0.1/usr/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}" go test -race -count=1 ./server`。
  2. 在 `LIMPET_HOME=~/.limpet-dev` 下按 `tests/README.md` 复跑相关端到端脚本（协议、多视图、备用屏重连、慢消费者、退出回收、浏览器重连），**清理仅针对本次测试创建的 UUID（严禁跑整库清空脚本误杀已有独立 dev worker）**。
  3. 对本地 `limpet` + 开发 server 做端到端实测验证后，列出待用户真机手测项目（`limpet codex` WSL 贴图、原生快捷键、手机端接管与 `attach`）。

---

## GPT 执行指令与汇报要求

1. **按顺序实施**：先做 U1 → U2 → U3（及 U4），每完成一个单元确保可编译且相关单测通过，全部完成后统一跑 U5 全量检查；按仓库 `AGENTS.md` 在完成可提交工作单元后使用规范格式自动提交（本地分支 `master`，不 push）。
2. **严格遵守防过度设计约束**：
   - 不新增 `go.mod` 依赖、不改前端代码、不加 `reserve` 租约与后台注册重试循环；
   - 不吞 `stderr`/错误根因，保持失败语义真实明确。
3. **汇报规范（沿用 `gpt-worker-handoff.md`）**：
   - 先给总结论与核心链路是否完整可用；
   - 任何异常、告警、跳过或未解析计数均带对应总量分母，区分已知覆盖边界与新发现问题；
   - 遇到真实技术卡点或需调整 Spec 边界的分支时先汇报停下，不擅自缩减 Spec 验收范围；
   - 全部实施与验收收口后，主动提醒用户开启新会话以免上下文压缩腐蚀。
