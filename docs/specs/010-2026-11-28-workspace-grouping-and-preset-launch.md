# Spec 010：侧边栏工作区聚合、预设快捷新建与列表样式优化

- 日期：2026-11-28
- 状态：已修订（完成 GPT 终轮复审前收口，待放行）
- 来源：用户希望提升 Web 端高频使用体验：左侧会话列表按项目工作路径（`workspace`）聚合、支持从顶栏与项目组头快捷选择启动 `Shell` / `Codex`（带 `--yolo`）/ `Agy`（带 `--dangerously-skip-permissions`）、自动汇总历史使用过的工作区路径、新建后直接进入终端，并同步精简优化侧边栏会话行的展示密度；同时明确本期暂不做自定义命令、收藏/置顶项目、搜索过滤、快捷键切换及已结束/harness 对话续接。
- 关联：延续并扩展 [Spec 003](003-2026-11-23-libghostty-worker-host.md)、[Spec 006](006-2026-11-26-local-cli-attach.md) 与 [Spec 007](007-2026-11-26-web-ui-pwa-and-lock-width.md)；保持 WebSocket 帧协议、Spec 009 回滚配置与录制 `.log`/`.meta` 格式不变。

## 目标

1. **Web 端预设快捷新建与历史目录直选**：支持在 Web 端按项目路径直接启动交互式 `Shell`、`codex --yolo` 或 `agy --dangerously-skip-permissions`，带目标路径/预设的新建自动按 `<目录名>-<命令名>` 命名并去重，建完立即打开进入终端。
2. **侧边栏工作区（`workspace`）聚合与样式瘦身**：将同一工作路径下的会话聚合为可折叠的工作区组（默认展示项目名、组头 `+` 一键秒建），移除每行冗余的路径文字，采用更清晰的紧凑两行卡片与悬停/触控自适应操作按钮。
3. **跨 `direct` 与 `systemd-run` 的一致行为**：预设 CLI 从用户的交互式登录环境取得 `PATH`，绕过同名别名和函数。缺失命令或就绪前失败时返回错误并回滚，server 重启恢复后保留 `cwd` 与 `cmd`。

## 一、后端预设启动、目录校验与 HTTP API 契约

### 1.1 闭合预设枚举、登录 Shell 支持范围与可执行预检

`POST /api/term` 支持可选字段 `preset`（与 `name`、`cwd` 均省略时严格兼容既有默认行为）：

- **`""` 或 `"shell"`（默认）**：
  - 沿用用户默认交互式 Shell 启动：`argv = [shell, "-i"]`（`shell` 沿用 `os.Getenv("SHELL")` 回退 `loginShell()`）；
  - 元数据 `cmd` 记为 `filepath.Base(shell)`。
- **`"codex"`**：
  - 使用交互式登录 Shell，`argv = [shell, "-lic", launchScript, shell, "codex", "--yolo"]`；
  - 元数据 `cmd` 记为 `"codex"`。
- **`"agy"`**：
  - 使用交互式登录 Shell，`argv = [shell, "-lic", launchScript, shell, "agy", "--dangerously-skip-permissions"]`；
  - 元数据 `cmd` 记为 `"agy"`。
- **交互式登录 Shell 兼容范围与 PATH 前提**：
  - `-lic` 预设仅支持 `sh`、`bash`、`zsh`、`dash`、`ksh`。若解析出的登录 Shell 不在此支持集合（如 `fish`），直接返回 `500` 与明确错误信息。
  - `-lic` 按 shell 的规则加载交互式登录环境。zsh 会加载 `.zprofile` 和 `.zshrc`（如存在），因此仅在 `.zshrc` 初始化的 nvm 或 `~/.local/bin` 也能进入 `PATH`。Bash 通过登录启动文件中已有的加载逻辑读取 `.bashrc`。
  - `launchScript` 固定为 `unset -f -- "$1" 2>/dev/null || true; exec "$@"`。默认 zsh 的 `exec` 可能解析到同名函数，因此先取消该函数。目标命令通过位置参数展开，避开同名别名，`exec` 用可执行文件替换 shell 进程。
  - **严禁**把工作路径 `cwd` 拼接进任何 Shell 命令字符串，工作目录仅通过子进程 `cmd.Dir` 设置。
- **目标可执行预检与快速退出边界**：
  - 启动 `"codex"` 或 `"agy"` 前，server 在与 PTY 等价的环境及目标 `cmd.Dir = cwd` 下执行一次 `5s` 超时的 `shell -lic` 预检。先执行 `unalias -a 2>/dev/null || true` 和 `unset -f -- "$1" 2>/dev/null || true`，再用 `command -v -- "$1"` 查找目标，校验结果非空且是可执行的普通文件（`-f` 与 `-x`）。
  - 若目标命令缺失或不可执行，**不拉起 worker**（也不空等 `5s` 就绪超时，更不返回假 `200`），`POST /api/term` 立即返回 `500 Internal Server Error` 与明确错误提示（如 `未在登录环境中找到 codex`）。
  - 若目标命令有效且拉起 worker 后在首次 `probe` 就绪前即退出（`ending` / worker 退出导致无法通过就绪探测），`POST /api/term` 同样返回 `500` 明确失败并清理残留，**不**登记活跃会话、**不**返回 `200`（此时尚未建立任何 WebSocket 连接，不适用 WS `1000`）。
  - 仅在会话通过就绪探测并有 viewer 接入后才进入运行期退出语义：之后目标进程退出向已接入 viewer 发送 WS 关闭码 `1000`（`ended`）并从活跃列表移除，不退回 Shell。
- 非枚举预设值一律返回 `400 Bad Request` `{"error": "..."}`，不启动任何进程。

### 1.2 `cwd` 规范化与校验

`POST /api/term` 接收可选字段 `cwd`：

- 空字符串或纯空白：规范化为用户主目录（`os.UserHomeDir()`）。
- `"~"` 或 `"~/..."`：将前缀 `~` 展开为用户主目录，随后经 `filepath.Clean` 规范化（不执行 `EvalSymlinks`，与 CLI `os.Getwd()` 路径口径一致；非空路径内的合法空格保留）。
- 其他非绝对路径、含 NUL 字节、目标路径在磁盘不存在或 `os.Stat` 显示不是目录（`!info.IsDir()`）：一律返回 `400 Bad Request` `{"error": "..."}`。

### 1.3 自动命名、旧请求兼容与重名去重

- **显式指定非空 `name`**：沿用既有 `nameValid` 校验（非空、≤100 rune、禁 `:` `.` 与控制字符，不合法 → `400`）与活跃会话/创建中名称唯一性检查（重名 → `409 Conflict`）。
- **旧版空请求兼容（`name`、`cwd`、`preset` 全省略或全为空白）**：严格保持既有 UUID 显示名（`name = id`），确保既有脚本与调用方（如无请求体或 `{}` 的 `POST /api/term`）100% 向后兼容。
- **带 `preset` 或 `cwd` 的自动命名与并发去重**：当 `name` 留空且显式提供了 `preset`（含 `"shell"`）或 `cwd` 时，按 CLI 一致规则生成名称并自动去重：
  - 若规范化后的 `cwd` 等于用户主目录（`$HOME`），基准名为 `cmd`（如 `zsh`、`codex`、`agy`）；
  - 否则基准名为 `filepath.Base(cwd) + "-" + cmd`（如 `limpet-codex`、`arbor-monitor-agy`）；
  - 将 `:` `.` `/` 与控制字符替换为 `-`，按 rune 截断至 `96` 字符；若结果仍不符合 `nameValid` 则回退为会话 `id`；
  - 在持有 `m.mu` 期间若基准名已被活跃或创建中会话占用，自动依次追加 `-2`、`-3`…（截断保证总长不超过 `100` rune，复用 `discover` 的后缀长度规则）直至可用并立即占位 `m.creating`，保证组头一键连点新建绝不因重名冲突失败。
- 前端顶栏新建弹窗与工作区组头 `+` 一键新建提交 `POST /api/term` 时，始终显式传递 `preset`（包括 `"shell"`、`"codex"`、`"agy"`）。

### 1.4 显式 `LIMPET_WEB_LAUNCH=1` 标记、差异化失败清理与注册表顺序

- **显式环境变量标记与命令行兼容性**：
  - Web `spawnWorkerDirect` 与 `spawnWorkerSystemdRun` 保持 worker 命令行参数严格为 `limpet-server worker <uuid>`（不改变既有进程匹配格式与 `TestMain` 入口契约），改由内部环境变量 **`LIMPET_WEB_LAUNCH=1`** 显式标识 Web 启动模式（与既有 `LIMPET_WORKER_CONFIG=1` 机制对称；`direct` 路径在继承环境附加 `LIMPET_WEB_LAUNCH=1`，`systemd-run` 路径通过 `--setenv=LIMPET_WEB_LAUNCH=1` 传入）。
  - worker 在 `startSession()` 拉起子进程 PTY 时使用 `cliEnv()`（剥离除 `LIMPET_HOME` 外的全部 `LIMPET_*` 变量，含 `LIMPET_WEB_LAUNCH`）+ `TERM=xterm-256color`，确保内部启动标记绝不泄漏进用户 Shell / Codex / Agy 进程。
  - CLI 的 fd 3 `LIMPET_WORKER_CONFIG=1` 路径及无标记的传统调用保持原契约不变。
- **严格失败关闭（Fail-Closed）与启动清理顺序**：
  - 在 `manager.create()` 拉起 worker 之前，先向 `0700` 的 `workers/` 目录原子写入 `0600` 的 `<uuid>.launch.json`（包含 `argv`、`cwd`、`cmd`、`name`）。
  - 检测到 `LIMPET_WEB_LAUNCH=1` 的 worker 必须成功读取、立即删除（`os.Remove`）并校验 `<uuid>.launch.json` 合法后才能启动 PTY；若该文件缺失、不可读、JSON 损坏或字段不合法，worker 必须记录错误并以非零退出码终止，**绝不允许**降级启动默认 Shell，也不得发布可接入的就绪 socket。
  - **差异化失败清理**：`manager.create()` 按启动结果区分清理步骤，杜绝向未成功创建的句柄解引用或与迟到 worker 竞态：
    1. `direct` 下 `cmd.Start()` 失败（`killHard == nil`，确认未启动进程）：直接删除 `<uuid>.launch.json`；
    2. `systemd-run` 返回错误（`killHard == nil`，临时单元提交状态不明）：先按本次 `uuid` 尽力执行 `systemctl --user stop limpet-worker-<uuid>.service` 收尾，再删除 `<uuid>.launch.json`；
    3. 已返回 `killHard` 但就绪等待超时或就绪前退出：先调用 `killHard()` 终止本次 worker 进程或 systemd 单元，再删除 `<uuid>.launch.json`（及残留 socket）。
  - **崩溃残留边界**：若 `limpet-server` 在原子写入 `.launch.json` 之后、拉起 worker 之前被 `SIGKILL` 强杀，残留的 `.launch.json` 不会被 `scanWorkers`（仅扫描 `*.sock`）识别为会话，也不会触发任何自动执行；定期清扫孤儿 `.launch.json` 不在本期范围。
  - **严格保留 `s.cli == false`**：由 `LIMPET_WEB_LAUNCH=1` 启动的 Web 会话保持 `s.cli == false` 且无需等待首个 viewer 即可 `probe` 就绪，保持既有 Web 多视图协同 resize 语义不变。
- **`probe` 显式 `cmd` 与注册表发布/回滚顺序**：
  - worker 从 `.launch.json` 保存显式 `name`、`cwd`、`cmd` 并由 `probe` 握手应答如实返回 `ack.Name`、`ack.Cwd`、`ack.Cmd`（严禁从 `[shell, "-lic", ...]` 的 `argv[0]` 推导而把 `codex`/`agy` 误报为 `zsh`）。
  - **先落盘后发布、`pendingIDs` 全程遮蔽**：`manager.create()` 在 worker `probe` 就绪后，**保持 `m.pendingIDs[id]` 占位期间**写入 `m.registry[id]` 并执行 `m.saveRegistry()`；
    - 仅当 `saveRegistry()` 返回 `nil` 时，在 `m.mu` 下写入 `m.sessions[id] = h` 并删除 `m.pendingIDs[id]` 对外发布；
    - 若 `saveRegistry()` 失败，在 `m.mu` 下撤销 `m.registry[id]`，**继续保持 `m.pendingIDs[id]` 占位**直到终止新拉起的 worker 并确认其 socket 已消失或不可拨号（`killWorker` 后等待 socket 清理/失效，超时则回退 `killHard()` 并移除 socket，**不能**仅凭收到 `killWorker` 应答就视为已关闭），全部收尾完成后才释放 `pendingIDs[id]` 并返回 `500 Internal Server Error`，从而彻底防止并发 `GET /api/term` 在关闭窗口重新发现并登记半死 worker。
  - `discover()` 重启恢复活 worker 时：若 `sessions.json` 中已有该会话登记的名称（含用户重命名结果），优先保留 `reg.Name`；当 `reg.Name`/`reg.Cwd`/`reg.Cmd` 缺失时用 `probe` 回报的对应字段补齐并持久化。

### 1.5 `POST /api/term` 返回体与历史工作区接口 `GET /api/workspaces`

- **`POST /api/term`**：向后兼容超集，返回 `200 OK`：
  ```json
  {
    "id": "0123456789abcdef",
    "name": "limpet-codex",
    "cwd": "~/program/limpet",
    "cmd": "codex"
  }
  ```
  其中 `cwd` 按 `GET /api/term` 统一口径将 `$HOME` 缩写为 `~` / `~/...`；原有调用方仅读取 `id` 保持完全兼容。
- **`GET /api/term`**：保持现有响应数组格式（`SessionItem[]`）不变。
- **`GET /api/workspaces`**：
  - 返回 `200 OK` JSON 字符串数组（按 `~/` 缩写格式化，如 `["~/program/solana/arbor-monitor", "~/program/limpet"]`）；
  - 汇总 `sessions.json`（含活跃与已结束会话）中非空且规范化后不等于 `$HOME` 的 `cwd`（未登记 `cwd` 的历史会话自然跳过），按规范化绝对路径去重并取最近一次会话的 `createdAt`；锁外校验目标目录当前在磁盘存在且为目录（`os.Stat` + `IsDir()`），按最近创建时间倒序排列，同时间戳按格式化路径升序稳定排序。

## 二、前端工作区聚合与侧边栏样式契约

### 2.1 工作区（`workspace`）聚合、标签消歧与折叠记忆

- **分组规则**：
  - 将 `sessions` 中无 `cwd`（含历史无目录的旧会话）或 `cwd === "~"` 的会话统一归入默认工作区「**主目录**」（分组键 `""`），其余相同 `cwd` 的会话聚合为一个工作区组；
  - 组间排序：所有具名项目工作区排在「主目录」之前，并按组内最新会话的创建时间（`created` 最大值）**倒序**排列（仅随会话增删变化，不因每 `5s` 轮询或 `attached` 状态变化导致分组跳动；同时间按路径稳定排序）；「主目录」组（若非空）固定置于末尾；
  - 组内排序：保持 `GET /api/term` 返回的既有顺序（创建时间升序）。
- **组标题与路径消歧**：
  - 默认展示路径末段项目名（如 `~/program/limpet` 显示 `limpet`、`~/program/solana/arbor-monitor` 显示 `arbor-monitor`）；
  - 当两个及以上不同工作区共享同一末段名时（如 `~/work/app` 与 `~/personal/app`），自动补充上一级父目录进行最小消歧（`work/app` 与 `personal/app`，逐级向上直至唯一或完整路径）；
  - 桌面细指针悬停组标题复用现有 `cwdTip` 显示完整工作路径。
- **折叠记忆与 DOM 兼容约束**：
  - 点击组头（不含右侧 `+` 按钮）可折叠/展开该工作区，折叠集合按完整显示路径持久化至 `localStorage`（键 `limpet.collapsed-workspaces`）；新建会话或打开终端切换到某个会话时，自动展开其所属工作区；
  - **既有回归选择器兼容**：每个工作区组必须采用语义化的 `<section>`（禁止嵌套外层 `<li>`），会话项仍是组内 `<ul><li>`，确保既有脚本依赖的 `li`、`li button`、`[title="重命名"]`、`[title="关闭"]` 与关闭中单行禁用语义全部保持兼容。

### 2.2 侧边栏会话行紧凑两行样式与 `#quick-switch` 同步

- **去除行内重复路径**：原行内重复展示的 `cwd` 已由工作区组头承担，会话行不再重复渲染工作路径。
- **紧凑两行内容**：
  - **第一行**：状态小圆点（使用中 `--limpet-accent` / 空闲 `--limpet-muted`）+ 会话名（单行 `truncate` 占满主宽）；
  - **第二行**：类型小徽章（`s.cmd`，为 `codex` 与 `agy` 提供柔和、低饱和度的暗黑区分色，普通 Shell 保持中性）+ 简短创建时间；
- **行尾操作按钮自适应**：
  - 桌面细指针（`@media (hover: hover) and (pointer: fine)`）下，行尾「重命名」「关闭」按钮常态保持 `opacity: 0`，当行处于 `:hover`、`:focus-within` 或当前选中（`.session-row-active`）时平滑显示为 `opacity: 1`；**严禁**使用 `display: none`、`visibility: hidden` 或 `pointer-events: none` 屏蔽隐藏态，以保持键盘 Tab 可达与既有自动化定位稳定可点击；
  - 触控设备（粗指针）保持操作按钮常显且热区 `≥ 44×44px`。
- **顶栏快速切换（`#quick-switch`）同步分组**：
  - `#quick-switch` 弹窗复用同一工作区聚合函数，以紧凑组标题分区展示各会话，方便切换时直观按项目定位。

## 三、快捷新建与自动打开交互契约

### 3.1 顶栏组合「新建」按钮

- 左侧主按钮：保留**唯一且精确**的可访问名称 `新建`（兼容既有 S10 测试），点击直接以默认预设 `"shell"` 打开「新建会话」弹窗；
- 右侧紧邻下拉触发按钮（`aria-label="更多新建选项"`）：打开轻量下拉菜单，列出三项预设：
  - **Shell**（默认交互式终端）
  - **Codex**（副标 `--yolo`）
  - **Agy**（副标 `--dangerously-skip-permissions`）
- 点选下拉项后关闭菜单，并以对应预设预选状态打开「新建会话」弹窗。

### 3.2 「新建会话」弹窗与非阻塞直开

- **预设选择**：弹窗顶部提供 `Shell` / `Codex` / `Agy` 分段或单选切换按钮组；
- **工作目录**：提供工作目录文本输入框（占位符默认主目录 `~`），下方展示从 `GET /api/workspaces` 获取的近期项目路径快捷标签（点击填入输入框，打开新建弹窗时触发一次 `GET /api/workspaces` 刷新，复用统一 `request()` 保留 `authExpired` 检测；接口异常不阻断手输或默认创建）；
- **会话名称**：保留原输入框与占位符 **`会话名（留空自动生成）`**；
- **提交与非阻塞直开**：调用 `POST /api/term` 传入 `{ name, cwd, preset }`，成功返回 `{ id, name, cwd, cmd }` 后关闭弹窗、用返回中的 `cwd` 立即展开目标工作区并**立即打开进入新创建的终端**（`emit('open', created.id, created.name)`），**不阻塞等待 `refresh()` 完成**，而是后台异步触发 `void refresh()`；后端返回错误时在弹窗内联展示错误信息，保留输入供修改。

### 3.3 工作区组头 `+` 一键秒建（无确认弹窗）

- 每个工作区组头右侧提供独立 `+` 按钮（`aria-label="在 <组名> 新建会话"`，点击不触发折叠，满足规范可点击热区）；
- 点击某组 `+` 按钮后弹出共享的轻量预设菜单（**Shell** / **Codex** `--yolo` / **Agy** `--dangerously-skip-permissions`）；
- 点选任意预设后**不弹确认弹窗**，直接发起 `POST /api/term`（传入该组完整 `cwd`、选中 `preset`、空 `name` 由后端按 `<项目>-<命令>` 自动命名并去重）；
- 以工作区完整 `cwd` 键记录进行中状态：同一工作区创建请求进行中禁止重复点击并在结束（成功/失败）时释放；成功后立即展开分组并**直接打开进入新终端**（`void refresh()` 后台异步刷新）；失败则在列表级错误提示区明确显示错误文案。

### 3.4 移动端 `360px` 防溢出

- `360px` 视口下，**整页文档以及侧边栏/列表容器内部**（包含长消歧项目标题、长路径、组合「新建」按钮、组头 `+` 菜单、触控常显操作按钮与「新建会话」弹窗）均不得产生横向溢出或内容裁切。

## 四、非目标

- 不支持自定义命令输入或外部命令模板编辑；
- 不做收藏或置顶工作区；
- 不做侧边栏搜索、过滤或键盘快捷键切换会话；
- 不做已结束会话列表展示、Shell 假续接或 harness（Codex/Agy）对话续接；
- 不改动 WebSocket 帧格式、终端输入输出语义、Spec 009 回滚配置或录制格式。

## 五、验收标准

### 5.1 后端 Go 单测（`go test -race -count=1 ./server`）

1. **预设、登录 Shell 语法、PATH 隔离、预检与就绪前退出边界**：
   - 验证 `""`/`"shell"`、`"codex"`、`"agy"` 均合法并产出预期 `cmd` 与 `argv`。Codex/Agy 经 `shell -lic` 取消同名函数后直替进程，未知 `preset` 返回 `400`，`cwd` 仅通过 `cmd.Dir` 传递；
   - 使用精简基础 `PATH`，仅在隔离的 `.zshrc` 加入 shim 目录，同时定义同名陷阱别名和函数。验证预检通过，PTY 加载了交互环境并按 `--yolo`/`--dangerously-skip-permissions` 实际运行 shim，陷阱均未触发；
   - 验证不支持的登录 Shell（如 `fish`）与目标命令缺失时，在拉起 worker 前立即返回 `500` 错误且不等待 5s 超时也不残留 worker；
   - **就绪前快速退出边界**：验证使用有效可执行 shim（拉起后立即正常退出 `exit 0`）在首次 `probe` 就绪前结束时，`POST /api/term` 返回 `500` 且不留下已登记会话或假 `200`。
2. **`cwd` 校验与规范化**：空/空白规范为 `$HOME`、`~` 与 `~/sub` 正确展开并 Clean、保留合法目录空格；相对路径、不存在路径、普通文件路径、含 NUL 字节路径一律 `400`。
3. **自动命名与兼容性**：
   - 验证空体或 `{}` 旧版 `POST /api/term` 保持 UUID 显示名；
   - 验证显式传 `preset` 或 `cwd` 且空 `name` 时在项目目录生成 `<dir>-<cmd>`、主目录生成 `<cmd>`，并发/连续创建自动去重 `-2`/`-3`（含超 `100` rune 截断），显式重名仍返回 `409`。
4. **`LIMPET_WEB_LAUNCH=1` 严格失败关闭、清理顺序、重启恢复与 `saveRegistry` 回滚**：
   - 验证 `LIMPET_WEB_LAUNCH=1` 下缺失、损坏或非法 `.launch.json` 时 worker 直接非零退出（绝不启动默认 Shell 或发布就绪 socket），子进程 PTY 环境中不包含 `LIMPET_WEB_LAUNCH`，且命令行维持 `worker <uuid>` 不变；
   - 验证 `.launch.json` 以 `0600` 创建、读取即删，不同失败路径（`direct` 启动失败、`systemd-run` 提交失败、就绪超时）均按顺序清理且不残留 `.launch.json`，Web 会话保持 `s.cli == false` 且无 viewer 即可就绪；
   - 验证 `saveRegistry()` 写入失败时在释放 `pendingIDs` 前阻止并发 `scanWorkers` 重新发现该 worker，待确认 worker/socket 已关闭清理后再释放 `pendingIDs` 并返回 `500`；
   - 验证 `sessions.json` 中缺失某活 `codex`/`agy` 条目时，重启恢复（`recoverWorkers`）可从 `probe` 准确找回 `cwd` 与显式 `cmd`（如 `"codex"`/`"agy"` 而非 `"zsh"`）；同时当 `sessions.json` 中已存在用户重命名名称时予以保留。
5. **`GET /api/workspaces`**：验证汇总存在 `cwd` 的历史/活跃记录、跳过缺失 `cwd` 的旧记录、过滤 `$HOME` 与已删除目录、去重并按最近 `createdAt` 倒序（同时间按路径升序）、输出 `~/` 缩写，同时 `GET /api/term` 保持既有数组结构。

### 5.2 前端构建、端到端 Playwright 与真实 `systemd-run --user` 验收

（说明：因涉及新建不同 `cwd` 与测试 `PATH` shim，验收使用本轮专用隔离目录 `.runtime/dev-home-010` 与空闲端口对 `PORT`/`PORT+2`，并按「按本轮 UUID 关闭会话 → 普通 `kill` 停测试 server → 清理本轮目录」执行收尾。）

1. **构建与既有回归基准（`LIMPET_WORKER_LAUNCH=direct`）**：
   - `cd web && pnpm build`（含 `vue-tsc -b`）通过，`go build -tags limpet_embed` 成功；
   - 传绝对路径 `SERVER_BIN`、专用 `LIMPET_HOME`、空闲 `PORT` 与 `LIMPET_WORKER_LAUNCH=direct` 运行 `tests/limet-reconnect-browser-test.mjs`，确认既有 **10/10 场景**（含 S8 按 `(^|/)<bin> worker <uuid>$` 精确匹配 PID）全部通过；U2 未改 `server/` 时可复用 U1 Go `-race` 基准。
2. **工作区聚合、消歧、折叠与 S10 DOM 兼容**：
   - 创建属于两个不同目录（含一对同名叶目录 `a/demo` 与 `b/demo`）、`~` 及旧无 `cwd` 会话，验证分组归属、同名父级消歧、「主目录」在后、点击折叠/刷新后记忆、打开/新建自动展开、`#quick-switch` 同步分组；
   - 验证会话行紧凑两行无重复路径、显示命令徽章、桌面细指针下行尾按钮静态 `opacity: 0` 但无 `pointer-events: none` 且在 hover/focus-within 下可点击（保留精确 `新建`、会话 `<li>`、`title="重命名"`、`title="关闭"`）。
3. **非阻塞自动进入、顶栏组合按钮与组头 `+` 秒建**：
   - 验证顶栏精确「新建」按钮直接打开默认 Shell 弹窗、右侧下拉展示 `Shell`/`Codex`/`Agy`、弹窗刷新 `GET /api/workspaces` 历史目录标签并可点选或手输；
   - **非阻塞验证**：刻意延迟 `GET /api/term` 刷新响应，验证通过顶栏或组头 `+` 提交 `POST /api/term` 成功后不等待列表刷新即展开分组并打开进入新终端；
   - 在某组头点 `+` 连选两次快速创建，验证无确认弹窗且自动 `-2` 去重、同组请求中防连点、后端错误展示列表告警。
4. **登录 PATH + 防别名套娃与 `360px` 内外无溢出**：
   - 在隔离测试环境的精简基础 `PATH` 下，仅通过 `.zshrc` 加入临时 `codex`/`agy` shim 目录并定义同名陷阱别名和函数。端到端验证通过网页创建的会话在目标 `cwd` 带正确参数启动，且陷阱均未触发；
   - 在 `360px` 视口下以长消歧项目名与长路径断言**文档与侧边栏容器内部均无横向溢出**，且粗指针下操作按钮热区合规。
5. **真实 `systemd-run --user` 专项验收**：
   - 在同一隔离临时目录以 `LIMPET_WORKER_LAUNCH=systemd-run` 启动内嵌二进制测试 server，创建一条指定 `cwd` 与 `codex`（或 `agy`）预设会话，确认其运行在独立 `limpet-worker-<uuid>.service` 临时单元、真实 `cwd` 与启动参数符合预期，且重启 test server 后能被完整找回（`cwd`/`cmd`/`name` 不丢）；最后按 UUID 关闭会话并停掉测试 server、清理临时单元与目录。
