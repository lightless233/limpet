# Plan 010：侧边栏工作区聚合、预设快捷新建与列表样式优化

- 日期：2026-11-28
- 对应 Spec：`docs/specs/010-2026-11-28-workspace-grouping-and-preset-launch.md`（已同步 GPT 终轮复审前修订，待放行）
- 执行方式：DELEGATED（适合转交 GPT / Codex 实施与验收，按 `U1 → U2` 顺序推进）
- 约束基线：
  - **环境隔离与上线红线**：Go 单测用 `t.TempDir()`；U2 浏览器与 `systemd-run` 专项验收因涉及不同 `cwd` 会话与临时 `PATH` shim，作为本轮对 `AGENTS.md` 通用 `~/.limpet-dev` 约定的显式隔离例外，统一使用**本轮专用隔离临时目录**（`.runtime/dev-home-010`）与独立空闲端口对（`PORT` 与 `PORT+2` 空闲），避免污染 `~/.limpet-dev` 既有会话；**严禁**执行 `deploy/install.sh`、**严禁**触碰 `/opt/limpet` 与正式 `~/.limpet`；不要加入仅服务于测试的生产开关后门。
  - **工具链环境变量**：编译与运行 Go 测试需带 `PKG_CONFIG_PATH="$HOME/.local/opt/limpet-toolchain/install-1.0.1/usr/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"`。
  - **进程清理安全**：清理时先逐一通过接口按本轮创建的会话 UUID 关闭会话，再用普通 `kill` 停掉自己起的临时测试 server（绝不使用按进程树整棵清理的方式），最后删除 `.runtime/dev-home-010`、`.runtime/limpet-server-010` 与本轮临时脚本；`pgrep -f`/`pkill -f` 若使用需遵循 `[x]yz` 形式。
  - **Git 规范**：遵循 `AGENTS.md`，每完成一个可提交单元自动 commit（英文 Conventional Commits，`Co-authored-by` 尾注按实际执行的 agent / 模型填写，拿不准先向用户确认，**不主动 push**）。

## 总体顺序与依赖关系

```text
U1（后端 preset/cwd 校验与预检、LIMPET_WEB_LAUNCH=1 + .launch.json 生命周期、先落盘后发布、GET /api/workspaces）
  └──▶ U2（前端 workspace 聚合与样式优化、非阻塞快捷新建与 S10 DOM 契约、隔离 direct + systemd-run 验收与清理）
```

## 实施单元

### U1：后端预设启动、目录校验、`launch.json` 传递与 `GET /api/workspaces`

- **目标**：扩展 `POST /api/term` 支持 `cwd` 与闭合预设 `shell` / `codex` / `agy`，补齐登录 Shell 支持判断、命令可执行预检、`LIMPET_WEB_LAUNCH=1` 严格失败关闭与就绪前退出边界、先落盘后发布与错误回滚，并新增 `GET /api/workspaces` 历史工作区接口。
- **涉及文件**：`server/main.go`、`server/handlers.go`、`server/manager.go`、`server/proto.go`、`server/launch.go`、`server/workerapp.go`、`server/session.go` 及 `server/*_test.go`。
- **关键改动与防坑点**：
  1. **闭合预设枚举、登录 Shell 检查与预检（`server/handlers.go`、`server/manager.go`、`server/session.go`）**：
     - 仅允许 `""` / `"shell"`、`"codex"`、`"agy"`；未知 `preset` 返回 `400`。
     - `"shell"` 沿用 `[shell, "-i"]`。Codex/Agy 使用 `[shell, "-lic", launchScript, shell, preset, flag]` 加载交互式登录环境，包括 zsh 的 `.zshrc` 中初始化的 `PATH`。脚本固定为 `unset -f -- "$1" 2>/dev/null || true; exec "$@"`，取消同名函数并通过位置参数绕过同名别名。参数分别为 `--yolo` 和 `--dangerously-skip-permissions`。支持 `sh`、`bash`、`zsh`、`dash`、`ksh`，其他 shell（如 `fish`）返回 `500`。
     - **目标可执行预检与快速退出边界**：启动 `codex`/`agy` 前，在与 PTY 等价的环境和目标 `cwd` 下执行 `5s` 超时的 `shell -lic` 预检。先清除别名和同名函数，再用 `command -v`、`-f` 与 `-x` 确认目标。缺失或不可执行时返回 `500`，不拉起 worker。有效目标若在首次 `probe` 就绪前退出，`POST /api/term` 返回 `500` 且不留登记。WS `1000` 仅用于接入之后退出。
     - **严禁**将 `cwd` 拼接到任何 Shell 命令行字符串中；`cwd` 仅通过 `cmd.Dir` 设置。
  2. **`cwd` 校验、旧 POST 兼容与自动命名去重（`server/handlers.go`、`server/manager.go`）**：
     - 规范化 `cwd`：空/纯空白→`$HOME`；`~`/`~/...` 展开并 `filepath.Clean`（不做 `EvalSymlinks`，保留非空有效空格）；非绝对路径、含 NUL、不存在或非目录（`os.Stat`）一律 `400`。
     - 显式 `name` 沿用 `nameValid` 与 `409` 重名检查；当 `name`、`cwd`、`preset` 全空/省略时保持既有 UUID 显示名兼容旧脚本；当 `name` 为空且 `preset != ""` 或 `cwd != ""` 时按 `<目录末段>-<cmd>`（主目录为 `<cmd>`）在 `m.mu` 内自动去重（`-2`/`-3`… 且截断保证总长 ≤ 100 rune）并用 `m.creating` 占位。
  3. **显式 `LIMPET_WEB_LAUNCH=1`、差异化清理与发布/回滚顺序（`server/launch.go`、`server/manager.go`、`server/workerapp.go`、`server/session.go`）**：
     - `spawnWorkerDirect`（Web 路径，基于 `os.Environ()` 保留测试变量并附加 `LIMPET_WEB_LAUNCH=1`）与 `spawnWorkerSystemdRun`（`--setenv=LIMPET_WEB_LAUNCH=1`）保持命令行参数严格为 `worker <uuid>`（不改动 `manager_test.go` 的 `TestMain` 参数检查与 S8 `pgrep` 匹配）。
     - worker 在 `startSession()` 中通过 `cliEnv()` 构建子进程 PTY `cmd.Env`（剥离 `LIMPET_WEB_LAUNCH` 等非 `LIMPET_HOME` 的 `LIMPET_*` 变量）+ `TERM=xterm-256color`。
     - `manager.create()` 原子写 `0600` `<uuid>.launch.json` 后再 spawn，清理按路径区分：`direct` `Start()` 失败（`killHard == nil`）直接删文件；`systemd-run` 报错（`killHard == nil`）先按 `uuid` 尽力停 `limpet-worker-<uuid>.service` 再删文件；就绪超时或就绪前失败（`killHard != nil`）先调 `killHard()` 再删文件。
     - 检测到 `LIMPET_WEB_LAUNCH=1` 的 worker 必须成功读取、删除并校验 `.launch.json` 才启动 PTY，缺失或损坏非零退出（不降级默认 Shell）；同时保留 `s.cli == false` 且无 viewer 即可 `probe` 就绪。
     - worker 显式保存并由 `probe` 返回 `ack.Name`/`ack.Cwd`/`ack.Cmd`（不从 `argv[0]` 误推 shell 名）；`manager.create()` **保持 `pendingIDs[id]` 占位期间先调用 `saveRegistry()`**，落盘成功后才写 `m.sessions[id]` 并删除 `pendingIDs[id]`；若 `saveRegistry()` 失败，撤销 `m.registry[id]`、保持 `pendingIDs[id]` 直至关闭本次 worker 并确认其 socket 已消失/不可拨号（`killWorker` 后等待 socket 失效，超时回退 `killHard()` + 删 socket）再释放 `pendingIDs[id]` 返回 `500`；`discover()` 恢复保留已有 `reg.Name` 并补齐缺失字段；`POST /api/term` 返回兼容超集 `{ id, name, cwd, cmd }`。
  4. **`GET /api/workspaces`（`server/main.go`、`server/handlers.go`、`server/manager.go`）**：
     - 短持 `m.mu` 汇总 `m.registry` 中非空且非 `$HOME` 的 `cwd`（按规范化路径取最大 `CreatedAt`），锁外 `os.Stat` 校验存在目录并按时间倒序（同时间按路径升序）输出 `~/` 缩写数组。
- **完成证据**：带 `PKG_CONFIG_PATH` 执行 `go test -race -count=1 ./server` 全部通过，覆盖 Spec 010 §5.1 全部单测项（含精简基础 PATH 下仅由 `.zshrc` 引入 shim 且绕过同名别名与函数、unsupported shell、缺失命令 500、就绪前退出 shim 返回 500、`LIMPET_WEB_LAUNCH=1` 失败关闭及环境剥离、`saveRegistry` 失败期间 `pendingIDs` 防重发现与回滚、无 registry 条目恢复显式 `cmd`、旧空体 POST 兼容与去重、`GET /api/workspaces`）。

### U2：前端工作区聚合、快捷新建、样式升级与隔离端到端验收

- **目标**：完成工作区聚合与消歧展示、侧边栏紧凑两行卡片与快速切换分组、顶栏组合「新建」与组头 `+` 一键秒建、非阻塞直开新终端，并在隔离环境完成真实 `direct` 与 `systemd-run --user` 端到端验收。
- **涉及文件**：`web/src/sessions.ts`、`web/src/components/SessionList.vue`、`web/src/components/TerminalView.vue`、`web/src/components/AppIcon.vue`（按需）、`web/src/App.vue`、`web/src/style.css`（不修改 `server/`，复用 U1 Go `-race` 基准）。
- **关键改动与防坑点**：
  1. **共享工作区聚合与 S10 DOM 兼容（`web/src/sessions.ts` / `SessionList.vue` / `TerminalView.vue`）**：
     - 抽出纯前端分组辅助函数供 `SessionList.vue` 与 `#quick-switch` 共用：无 `cwd` 或 `"~"` 归「主目录」排末尾；具名工作区按组内最新会话 `created` 倒序（同时间按路径稳定排序）；同末段名自动向上逐级消歧，完整路径由 `cwdTip` 悬停展示；`localStorage`（`limpet.collapsed-workspaces`）记忆折叠态并在新建/打开时自动展开。
     - 保持工作区组外层用 `<section>`、会话行用 `<ul><li>`，保留精确唯一 `新建` 按钮、`title="重命名"`、`title="关闭"` 及关闭中单行禁用；桌面细指针下操作按钮用 `opacity: 0 → 1`（`:hover`/`:focus-within`/`.session-row-active`），**禁止**配合 `pointer-events: none`、`display: none` 或 `visibility: hidden`；粗指针保持常显与 `≥44px` 热区。
  2. **非阻塞自动进入、顶栏组合按钮与组头 `+`（`SessionList.vue`、`App.vue`）**：
     - 顶栏左主按钮精确为 `新建`（显式传 `preset: 'shell'` 打开弹窗），右侧 `更多新建选项` 菜单展示 `Shell` / `Codex`（`--yolo`）/ `Agy`（`--dangerously-skip-permissions`）；弹窗内可切预设、点选 `GET /api/workspaces` 历史路径或手输路径，保留原占位符 `会话名（留空自动生成）`。
     - `POST /api/term` 成功返回 `{ id, name, cwd, cmd }` 后，**不阻塞等待 `await refresh()`**，直接关闭弹窗/菜单、用响应的 `cwd` 展开工作区并立即 `emit('open', created.id, created.name)`，同时后台 `void refresh()`。
     - 组头右侧 `+` 打开共享轻量预设菜单，点选即按该组完整 `cwd` + `preset` 直接创建（无确认弹窗），按完整 `cwd` 键记录进行中状态防连点，错误显示在列表提示区。
  3. **隔离构建、浏览器回归、`systemd-run` 专项与收尾清理**：
     - 执行 `cd web && pnpm build`，带 `PKG_CONFIG_PATH` 编译 `go build -tags limpet_embed -o .runtime/limpet-server-010 ./server`。
     - **既有 S1–S10 回归**：传绝对路径 `SERVER_BIN="$PWD/.runtime/limpet-server-010"`、`LIMPET_HOME="$PWD/.runtime/dev-home-010"`、空闲 `PORT`（`PORT` 与 `PORT+2` 空闲）与 `LIMPET_WORKER_LAUNCH=direct` 运行 `node tests/limet-reconnect-browser-test.mjs`，确认 **10/10 场景**（因 `worker <uuid>` 参数保持不变，S8 `pgrep` 无需改动）全部通过。
     - **Spec 010 §5.2 专项 Playwright 验收**：在独立临时目录跑通分组/同名消歧/折叠与 `#quick-switch`、延迟 `GET /api/term` 验证非阻塞立即直开、组头 `+` 秒建与 `-2` 去重、精简基础 PATH 下仅在隔离 `.zshrc` 引入 shim 并定义陷阱别名和函数，验证 `codex --yolo` 与 `agy --dangerously-skip-permissions` 真实启动及 `cwd`、桌面悬停按钮可点、`360px` 下**整页与侧边栏容器内部均无横向溢出**。
     - **真实 `systemd-run --user` 专项验收**：在同一隔离临时目录以 `LIMPET_WORKER_LAUNCH=systemd-run` 启动内嵌 server，创建预设会话并核对独立 `limpet-worker-<uuid>.service` 单元、真实 `cwd` 与参数、以及重启 server 后 `recoverWorkers` 恢复全部元数据。
     - **清理顺序**：按 UUID 关闭本轮剩余测试会话 → 普通 `kill` 停测试 server → 彻底删除 `.runtime/dev-home-010`、`.runtime/limpet-server-010` 与本轮临时脚本（确保工作区干净、U2 无 `server/` 改动、`~/.limpet-dev` 与 `~/.limpet` 未受触碰）。
- **完成证据**：`cd web && pnpm build` PASS、U1 `go test -race -count=1 ./server` 基准有效、既有 `10/10` + Spec 010 §5.2 Playwright + `systemd-run --user` 专项全部通过，仓库干净无残留。

---

## GPT 执行与汇报要求

1. 严格按 `U1 → U2` 顺序实施；遵循 `AGENTS.md`，每完成一个可提交工作单元自动 `commit`（英文 Conventional Commits + 真实执行的 agent/模型 `Co-authored-by` 尾注，拿不准先向用户确认），**禁止主动 push**、**禁止触碰 `~/.limpet`**、**禁止执行 `deploy/install.sh`**，停自启临时 server 只用普通 `kill`。
2. 执行与最终汇报遵循 [gpt-worker-handoff.md](file:///home/lightless/.gemini/config/rules/gpt-worker-handoff.md)：
   - **先给业务结论与验证状态**，再展开明细，严禁堆砌免责声明；
   - **所有非零异常、告警、跳过或未解析计数必须带总量分母**，并明确区分既有覆盖边界与本轮新增问题；
   - 遇到真实技术卡点或契约分叉时先用通俗语言说明原因、方案代价与推荐，未经同意不得擅自缩减范围；
   - 单项复测按最小缺口验证，不机械重跑无关全量测试；
   - 全部单元完成并收尾后，**主动提醒用户开启新的 Codex 会话**。
