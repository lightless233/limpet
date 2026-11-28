# Plan 010：侧边栏工作区聚合、预设快捷新建与列表样式优化

- 日期：2026-11-28
- 对应 Spec：`docs/specs/010-2026-11-28-workspace-grouping-and-preset-launch.md`（待 GPT 预审）
- 执行方式：DELEGATED（适合转交 GPT / Codex 实施与验收，按 `U1 → U2` 顺序推进）
- 约束基线：
  - **环境隔离与上线红线**：Go 单测用 `t.TempDir()`，U2 浏览器验收使用**本轮专用隔离临时目录**（如 `.runtime/dev-home-010`）与独立空闲端口，避免污染或改写 `~/.limpet-dev` 既有会话；**严禁**执行 `deploy/install.sh`、**严禁**触碰 `/opt/limpet` 与正式 `~/.limpet`；不要加入仅服务于测试的生产开关后门。
  - **工具链环境变量**：编译与运行 Go 测试需带 `PKG_CONFIG_PATH="$HOME/.local/opt/limpet-toolchain/install-1.0.1/usr/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"`。
  - **进程清理安全**：停掉自己起的临时测试 server 只用普通 `kill`（绝不使用按进程树整棵清理的方式）；`pgrep -f`/`pkill -f` 若使用需遵循 `[x]yz` 形式。
  - **Git 规范**：遵循 `AGENTS.md`，每完成一个可提交单元自动 commit（英文 Conventional Commits，`Co-authored-by` 尾注按实际执行的 agent / 模型填写，拿不准先向用户确认，**不主动 push**）。

## 总体顺序与依赖关系

```text
U1（后端 preset/cwd 校验与自动命名、一次性 .launch.json 启动配置、probe/registry 保留与 GET /api/workspaces）
  └──▶ U2（前端 workspace 聚合与样式优化、顶栏/组头快捷新建与自动进入、隔离环境 limpet_embed 验收与清理）
```

## 实施单元

### U1：后端预设启动、目录校验、`launch.json` 传递与 `GET /api/workspaces`

- **目标**：扩展 `POST /api/term` 支持 `cwd` 与闭合预设 `shell` / `codex` / `agy`，打通 `direct` 与 `systemd-run` 两种 worker 拉起路径的参数传递与重启恢复元数据，并新增 `GET /api/workspaces` 历史工作区接口。
- **涉及文件**：`server/main.go`、`server/handlers.go`、`server/manager.go`、`server/proto.go`、`server/launch.go`、`server/workerapp.go`、`server/session.go` 及 `server/*_test.go`。
- **关键改动与防坑点**：
  1. **闭合预设枚举与无别名泄漏启动（`server/handlers.go` / `server/manager.go`）**：
     - 仅允许 `""` / `"shell"`、`"codex"`、`"agy"`；未知 `preset` 返回 `400`。
     - `"shell"` 沿用 `[shell, "-i"]`；`"codex"` 转为 `[shell, "-lc", "exec \"$@\"", shell, "codex", "--yolo"]`；`"agy"` 转为 `[shell, "-lc", "exec \"$@\"", shell, "agy", "--dangerously-skip-permissions"]`（既绕过 `systemd --user` 精简 `PATH` 问题，又不读 `.zshrc` 交互别名，避免 `alias codex='limpet codex'` 自递归）。
     - **严禁**将 `cwd` 拼接到命令行字符串中；`cwd` 仅通过 `cmd.Dir` 设置。
  2. **`cwd` 校验与自动命名去重（`server/handlers.go`、`server/manager.go`）**：
     - 解析并规范化 `cwd`：空→`$HOME`、`~`/`~/...` 展开并 `filepath.Clean`（不做 `EvalSymlinks`）；非绝对路径、含 NUL、不存在或非目录（`os.Stat`）一律 `400`。
     - 显式 `name` 沿用 `nameValid` 与 `409` 重名检查；留空时在 `m.mu` 临界区内按 `<目录末段>-<cmd>`（主目录则为 `<cmd>`）生成基准名，清洗 `:` `.` `/` 与控制字符、截断至 96 rune、自动递增 `-2`/`-3` 去重并用 `m.creating` 占位，保证高频连点秒建绝不重名报错。
  3. **Web 一次性 `.launch.json` 与元数据保留（`server/manager.go`、`server/workerapp.go`、`server/session.go`）**：
     - Web `manager.create()` 在 `workers/` 下通过原子写写入 `0600` 的 `<uuid>.launch.json`（含 `argv`、`cwd`、`cmd`、`name`），worker 启动读取后立即删除，并在 `manager.create()` 用 `defer` 清理兜底失败路径；若 `.launch.json` 存在却损坏则 worker 报错退出。
     - 区分于 CLI 的 `workerConfig.WaitFirstViewer`：Web 会话继续保持 `s.cli == false` 与立即可探测就绪语义，仅从启动配置接收 `argv`/`cwd`/`cmd`/`name`。
     - worker 在 `workerHost` 持有 `cwd`/`cmd`/`name` 并在 `probe` 应答返回，创建完成即把 `cwd`（原始绝对路径）与 `cmd` 写入 `sessions.json`；`POST /api/term` 返回兼容超集 `{ id, name, cwd, cmd }`。
  4. **`GET /api/workspaces`（`server/main.go`、`server/handlers.go`、`server/manager.go`）**：
     - 在短持 `m.mu` 读取 `m.registry` 快照（按 `CreatedAt` 汇总各非 `$HOME` 目录的最近使用时间）后，在锁外执行磁盘 `os.Stat` 校验与 `~/` 格式化，返回倒序去重的 `string[]`。
- **完成证据**：带 `PKG_CONFIG_PATH` 执行 `go test -race -count=1 ./server` 全部通过，覆盖 Spec 010 §5.1 全部单测项。

### U2：前端工作区聚合、快捷新建、样式升级与隔离 `limpet_embed` 验收

- **目标**：完成工作区聚合与消歧展示、侧边栏紧凑两行卡片与快速切换分组、顶栏组合「新建」与组头 `+` 一键秒建、建完自动打开进入终端，并在隔离环境完成真实内嵌二进制回归与专项验收。
- **涉及文件**：`web/src/sessions.ts`、`web/src/components/SessionList.vue`、`web/src/components/TerminalView.vue`、`web/src/components/AppIcon.vue`（如需补充极简线性图标）、`web/src/App.vue`、`web/src/style.css`（不修改 `server/`，复用 U1 Go `-race` 基准）。
- **关键改动与防坑点**：
  1. **共享工作区聚合与 DOM 选择器兼容（`web/src/sessions.ts` / `SessionList.vue` / `TerminalView.vue`）**：
     - 抽出一份纯前端分组辅助函数供 `SessionList.vue` 与 `TerminalView.vue` 的 `#quick-switch` 共用：无 `cwd` 或 `"~"` 归「主目录」排末尾；具名工作区按组内最新会话 `created` 倒序；同末段名自动向上逐级消歧，完整路径由 `cwdTip` 悬停展示。
     - 侧边栏分组外层必须用 `<section>`，保持会话行仍是组内 `<ul><li>`，不破坏既有浏览器测试的 `li` / `li button` 定位；桌面细指针下操作按钮用 `opacity: 0 → 1`（`:hover`/`:focus-within`/`.session-row-active`）而非 `display: none`，确保键盘可达且不破坏现有点击测试；粗指针保持常显与 `≥44px` 热区。
     - 折叠状态持久化 `limpet.collapsed-workspaces`，新建或打开会话时自动展开所属工作区。
  2. **顶栏组合「新建」、弹窗与组头 `+` 一键秒建（`SessionList.vue`、`App.vue`）**：
     - 顶栏左主按钮精确保持可访问名 `新建`（直接打开默认 `Shell` 弹窗），右侧紧邻 `更多新建选项` 菜单列出 `Shell` / `Codex` / `Agy`；新建弹窗内可切预设、选 `GET /api/workspaces` 返回的近期项目路径或手输路径，保留原会话名占位符 `会话名（留空自动生成）`。
     - 工作区组头右侧 `+` 按钮打开共享的轻量预设菜单（无确认弹窗），点选即直调 `POST /api/term` 自动命名并去重，每组请求进行中防连点，失败明确提示错误；顶栏与组头新建成功后均刷新列表并立即 `emit('open', created.id, created.name)` 进入新终端。
     - 验证 `360px` 窄屏下无横向溢出。
  3. **隔离 `limpet_embed` 构建、回归验收与清理**：
     - 执行 `cd web && pnpm build`，带 `PKG_CONFIG_PATH` 编译 `go build -tags limpet_embed -o .runtime/limpet-server-010 ./server`。
     - 使用**专用临时目录**（`LIMPET_HOME="$PWD/.runtime/dev-home-010"`）与独立空闲端口运行：
       - `tests/limet-reconnect-browser-test.mjs` 全套 **10/10 既有场景**；
       - Spec 010 §5.2 专项 Playwright 验收（工作区分组与同名消歧、折叠记忆、紧凑两行与悬停操作、顶栏组合按钮与历史工作区填充、组头 `+` 一键秒建及连点去重、使用临时 shim 验证 `codex --yolo` 与 `agy --dangerously-skip-permissions` 真实启动及 `cwd`、`#quick-switch` 同步分组、`360px` 零横向溢出）。
     - 普通 `kill` 停掉自启测试 server，彻底删除 `.runtime/dev-home-010`、`.runtime/limpet-server-010` 与临时验收脚本，保持工作区干净且 U2 无 `server/` 改动。
- **完成证据**：`cd web && pnpm build` PASS、U1 `go test -race -count=1 ./server` 基准有效、既有 `10/10` 与 Spec 010 §5.2 专项 Playwright 全部通过，仓库干净无残留。

---

## GPT 执行与汇报要求

1. 严格按 `U1 → U2` 顺序实施；遵循 `AGENTS.md`，每完成一个可提交工作单元自动 `commit`（英文 Conventional Commits + 真实执行的 agent/模型 `Co-authored-by` 尾注，拿不准先向用户确认），**禁止主动 push**、**禁止触碰 `~/.limpet`**、**禁止执行 `deploy/install.sh`**，停自启临时 server 只用普通 `kill`。
2. 执行与最终汇报遵循 [gpt-worker-handoff.md](file:///home/lightless/.gemini/config/rules/gpt-worker-handoff.md)：
   - **先给业务结论与验证状态**，再展开明细，严禁堆砌免责声明；
   - **所有非零异常、告警、跳过或未解析计数必须带总量分母**，并明确区分既有覆盖边界与本轮新增问题；
   - 遇到真实技术卡点或契约分叉时先用通俗语言说明原因、方案代价与推荐，未经同意不得擅自缩减范围；
   - 单项复测按最小缺口验证，不机械重跑无关全量测试；
   - 全部单元完成并收尾后，**主动提醒用户开启新的 Codex 会话**。
