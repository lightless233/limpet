# Spec 010：侧边栏工作区聚合、预设快捷新建与列表样式优化

- 日期：2026-11-28
- 状态：草案（待 GPT 预审）
- 来源：用户希望提升 Web 端高频使用体验：左侧会话列表按项目工作路径（`workspace`）聚合、支持从顶栏与项目组头快捷选择启动 `Shell` / `Codex`（带 `--yolo`）/ `Agy`（带 `--dangerously-skip-permissions`）、自动汇总历史使用过的工作区路径、新建后直接进入终端，并同步精简优化侧边栏会话行的展示密度；同时明确本期暂不做自定义命令、收藏/置顶项目、搜索过滤、快捷键切换及已结束/harness 对话续接。
- 关联：延续并扩展 [Spec 003](003-2026-11-23-libghostty-worker-host.md)、[Spec 006](006-2026-11-26-local-cli-attach.md) 与 [Spec 007](007-2026-11-26-web-ui-pwa-and-lock-width.md)；保持 WebSocket 帧协议、Spec 009 回滚配置与录制 `.log`/`.meta` 格式不变。

## 目标

1. **Web 端预设快捷新建与历史目录直选**：支持在 Web 端按项目路径直接启动交互式 `Shell`、`codex --yolo` 或 `agy --dangerously-skip-permissions`，自动按 `<目录名>-<命令名>` 命名并去重，建完立即打开进入终端。
2. **侧边栏工作区（`workspace`）聚合与样式瘦身**：将同一工作路径下的会话聚合为可折叠的工作区组（默认展示项目名、组头 `+` 一键秒建），移除每行冗余的路径文字，采用更清晰的紧凑两行卡片与悬停/触控自适应操作按钮。
3. **跨 `direct` 与 `systemd-run` 的一致行为**：无论服务以开发环境还是正式 `systemd --user` 运行，预设 CLI 均能完整解析用户登录 `PATH` 而不触发交互式 Shell 别名自递归，并在 server 重启恢复后保留完整的 `cwd` 与 `cmd`。

## 一、后端预设启动、目录校验与 HTTP API 契约

### 1.1 闭合预设枚举与进程启动语义

`POST /api/term` 支持可选字段 `preset`（与 `name`、`cwd` 均省略时严格兼容既有默认行为）：

- **`""` 或 `"shell"`（默认）**：
  - 按用户默认 Shell 启动交互式 Shell：`argv = [shell, "-i"]`（`shell` 沿用 `os.Getenv("SHELL")` 回退 `loginShell()`）；
  - 元数据 `cmd` 记为 `filepath.Base(shell)`。
- **`"codex"`**：
  - 通过非交互登录 Shell 解析完整环境并直替进程：`argv = [shell, "-lc", "exec \"$@\"", shell, "codex", "--yolo"]`；
  - 元数据 `cmd` 记为 `"codex"`。
- **`"agy"`**：
  - 通过非交互登录 Shell 解析完整环境并直替进程：`argv = [shell, "-lc", "exec \"$@\"", shell, "agy", "--dangerously-skip-permissions"]`；
  - 元数据 `cmd` 记为 `"agy"`。
- **环境与别名边界说明**：
  - 正式部署服务运行在 `systemd --user` 下默认 `PATH` 不包含 `~/.nvm/...` 或 `~/.local/bin`；而用户交互式 `~/.zshrc` 中常配置了 `alias codex='limpet codex'` 等快捷别名。采用非交互登录 Shell（`-lc` 配合位置参数 `exec "$@"`）既读取登录态完整 `PATH`，又跳过交互别名文件防止 worker 嵌套套娃，且 `exec` 替换自身不残留多余壳进程。
  - **严禁**把工作路径 `cwd` 拼接进任何 Shell 命令字符串，工作目录仅通过子进程 `cmd.Dir` 设置。
  - 非枚举预设值一律返回 `400 Bad Request` `{"error": "..."}`，不启动任何进程。
  - 退出行为维持现状：目标命令退出即结束会话（WS `1000`）并从活跃列表移除，不退回 Shell。

### 1.2 `cwd` 规范化与校验

`POST /api/term` 接收可选字段 `cwd`：

- 空字符串或纯空白：规范化为用户主目录（`os.UserHomeDir()`）。
- `"~"` 或 `"~/..."`：将前缀 `~` 展开为用户主目录，随后经 `filepath.Clean` 规范化（不执行 `EvalSymlinks`，与 CLI `os.Getwd()` 路径口径一致）。
- 其他非绝对路径、含 NUL 字节、目标路径在磁盘不存在或 `os.Stat` 显示不是目录（`!info.IsDir()`）：一律返回 `400 Bad Request` `{"error": "..."}`。

### 1.3 自动命名与重名处理

- **显式指定非空 `name`**：沿用既有 `nameValid` 校验（非空、≤100 rune、禁 `:` `.` 与控制字符，不合法 → `400`）与活跃会话/创建中名称唯一性检查（重名 → `409 Conflict`）。
- **`name` 留空或仅空白**：按 CLI 一致规则生成默认名称并自动去重：
  - 若规范化后的 `cwd` 等于用户主目录（`$HOME`），基准名为 `cmd`（如 `zsh`、`codex`、`agy`）；
  - 否则基准名为 `filepath.Base(cwd) + "-" + cmd`（如 `limpet-codex`、`arbor-monitor-agy`）；
  - 将 `:` `.` `/` 与控制字符替换为 `-`，按 rune 截断至 `96` 字符；若结果仍不符合 `nameValid` 则回退为会话 `id`；
  - 在持有 `m.mu` 期间若基准名已被活跃或创建中会话占用，自动依次追加 `-2`、`-3`… 直至可用并立即占位 `m.creating`，保证组头一键连点新建绝不因重名冲突失败。

### 1.4 一次性 Web 启动配置与恢复元数据

- **启动参数传递（兼容 `direct` 与 `systemd-run`）**：
  - Web `manager.create()` 在拉起 worker 之前，向权限 `0700` 的 `workers/` 目录写入 `0600` 的一次性 `<uuid>.launch.json`（包含 `argv`、`cwd`、`cmd`、`name`），worker 启动时读取并立即删除该文件；`manager.create()` 同时注册 `defer` 删除该文件以兜底启动失败或超时；
  - `scanWorkers` 仅扫描 `*.sock`，忽略 `.launch.json`；若 `.launch.json` 存在但损坏/不合法，worker 明确报错退出，绝不静默降级为普通默认 Shell；
  - CLI `start` 原有的 fd 3 `workerConfig` 管道机制保持不变；
  - **严格保留 `s.cli` 语义区分**：通过 `.launch.json` 启动的 Web 会话保持 `s.cli == false`（不改变既有 Web 多视图协同 resize 语义，仅允许非显式预设配置传入初始 `argv`/`cwd`/`cmd`/`name`）。
- **重启找回元数据**：
  - worker 保存来自 `.launch.json` 的 `name`/`cwd`/`cmd` 并通过既有 `probe` 握手应答暴露给 server，使 server 重启扫描或跨重连找回活会话时完整保留工作区与命令标识；
  - 创建成功时同步将原始绝对路径 `cwd` 与 `cmd` 落盘到 `sessions.json`。

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
  其中 `cwd` 按 `GET /api/term` 统一口径将 `$HOME` 缩写为 `~` / `~/...`。
- **`GET /api/term`**：保持现有响应数组格式（`SessionItem[]`）不变。
- **`GET /api/workspaces`**：
  - 返回 `200 OK` JSON 字符串数组（按 `~/` 缩写格式化，如 `["~/program/solana/arbor-monitor", "~/program/limpet"]`）；
  - 汇总 `sessions.json`（含活跃与已结束会话）中的所有 `cwd`：过滤空值、过滤等于 `$HOME` 的路径、按规范化绝对路径去重、校验目标目录当前在磁盘仍存在且为目录（`os.Stat` + `IsDir()`），并按该目录最近一次会话 `createdAt` 倒序排列。

## 二、前端工作区聚合与侧边栏样式契约

### 2.1 工作区（`workspace`）聚合、标签消歧与折叠记忆

- **分组规则**：
  - 将 `sessions` 中无 `cwd` 或 `cwd === "~"` 的会话统一归入默认工作区「**主目录**」（分组键 `""`），其余相同 `cwd` 的会话聚合为一个工作区组；
  - 组间排序：所有具名项目工作区排在「主目录」之前，并按组内最新会话的创建时间（`created` 最大值）**倒序**排列（仅随会话增删变化，不因每 `5s` 轮询或 `attached` 状态变化导致分组跳动）；「主目录」组（若非空）固定置于末尾；
  - 组内排序：保持 `GET /api/term` 返回的既有顺序（创建时间升序）。
- **组标题与路径消歧**：
  - 默认展示路径末段项目名（如 `~/program/limpet` 显示 `limpet`、`~/program/solana/arbor-monitor` 显示 `arbor-monitor`）；
  - 当两个及以上不同工作区共享同一末段名时（如 `~/work/app` 与 `~/personal/app`），自动补充上一级父目录进行最小消歧（`work/app` 与 `personal/app`，逐级向上直至唯一或完整路径）；
  - 桌面细指针悬停组标题复用现有 `cwdTip` 显示完整工作路径。
- **折叠记忆与 DOM 兼容约束**：
  - 点击组头（不含右侧 `+` 按钮）可折叠/展开该工作区，折叠集合按完整显示路径持久化至 `localStorage`（键 `limpet.collapsed-workspaces`）；新建会话或打开终端切换到某个会话时，自动展开其所属工作区；
  - **既有回归选择器兼容**：每个工作区组必须采用语义化的 `<section>`（禁止嵌套外层 `<li>`），会话项仍是组内 `<ul><li>`，确保既有脚本依赖的 `li` / `li button` 定位全部保持兼容。

### 2.2 侧边栏会话行紧凑两行样式与 `#quick-switch` 同步

- **去除行内重复路径**：原行内重复展示的 `cwd` 已由工作区组头承担，会话行不再重复渲染工作路径。
- **紧凑两行内容**：
  - **第一行**：状态小圆点（使用中 `--limpet-accent` / 空闲 `--limpet-muted`）+ 会话名（单行 `truncate` 占满主宽）；
  - **第二行**：类型小徽章（`s.cmd`，为 `codex` 与 `agy` 提供柔和、低饱和度的暗黑区分色，普通 Shell 保持中性）+ 简短创建时间；
- **行尾操作按钮自适应**：
  - 桌面细指针（`@media (hover: hover) and (pointer: fine)`）下，行尾「重命名」「关闭」按钮常态保持 `opacity: 0`，当行处于 `:hover`、`:focus-within` 或当前选中（`.session-row-active`）时平滑显示为 `opacity: 1`；**禁止**使用 `display: none` 或 `visibility: hidden` 隐藏按钮，以保持键盘 Tab 可达与既有自动化定位稳定；
  - 触控设备（粗指针）保持操作按钮常显且热区 `≥ 44×44px`。
- **顶栏快速切换（`#quick-switch`）同步分组**：
  - `#quick-switch` 弹窗复用同一工作区聚合函数，以紧凑组标题分区展示各会话，方便切换时直观按项目定位。

## 三、快捷新建与自动打开交互契约

### 3.1 顶栏组合「新建」按钮

- 左侧主按钮：保留精确可访问名称 `新建`（`aria-label` 或按钮文本精确为 `新建`，兼容既有测试），点击直接以默认预设 `Shell` 打开「新建会话」弹窗；
- 右侧紧邻下拉触发按钮（`aria-label="更多新建选项"`）：打开轻量下拉菜单，列出三项预设：
  - **Shell**（默认交互式终端）
  - **Codex**（副标 `--yolo`）
  - **Agy**（副标 `--dangerously-skip-permissions`）
- 点选下拉项后关闭菜单，并以对应预设预选状态打开「新建会话」弹窗。

### 3.2 「新建会话」弹窗

- **预设选择**：弹窗顶部提供 `Shell` / `Codex` / `Agy` 分段或单选切换按钮组；
- **工作目录**：提供工作目录文本输入框（占位符默认主目录 `~`），下方展示从 `GET /api/workspaces` 获取的近期项目路径快捷标签（点击填入输入框，打开新建弹窗时触发一次 `GET /api/workspaces` 刷新，复用统一 `request()` 保留 `authExpired` 检测；接口异常不阻断手输或默认创建）；
- **会话名称**：保留原输入框与占位符 **`会话名（留空自动生成）`**；
- **提交后行为**：调用 `POST /api/term` 传入 `{ name, cwd, preset }`，成功返回后关闭弹窗、刷新会话列表、确保目标工作区展开，并**自动打开进入新创建的终端**（`emit('open', created.id, created.name)`）；后端返回 400/409/500 错误时在弹窗内联展示错误信息，保留输入供修改。

### 3.3 工作区组头 `+` 一键秒建（无确认弹窗）

- 每个工作区组头右侧提供独立 `+` 按钮（`aria-label="在 <组名> 新建会话"`，点击不触发折叠且仅在桌面或触控都保持清晰可点与 ≥32/44px 规范尺寸）；
- 点击某组 `+` 按钮后在附近弹出共享的轻量预设菜单（**Shell** / **Codex** `--yolo` / **Agy** `--dangerously-skip-permissions`）；
- 点选任意预设后**不弹确认弹窗**，直接发起 `POST /api/term`（传入该组 `cwd`、选中 `preset`、空 `name` 由后端按 `<项目>-<命令>` 自动命名并去重）；同一工作区组创建请求进行中防重复触发（并给出加载反馈），成功后自动展开该工作区、刷新列表并**立即打开进入新终端**；失败则在列表顶部错误提示区显示原因。

### 3.4 移动端 `360px` 防溢出

- `360px` 视口下，侧边栏组合「新建」按钮、工作区组头与标题/数量/`+` 按钮、快捷菜单、紧凑会话行及「新建会话」弹窗均不得产生页面横向溢出。

## 四、非目标

- 不支持自定义命令输入或外部命令模板编辑；
- 不做收藏或置顶工作区；
- 不做侧边栏搜索、过滤或键盘快捷键切换会话；
- 不做已结束会话列表展示、Shell 假续接或 harness（Codex/Agy）对话续接；
- 不改变 WebSocket 帧格式、终端输入输出语义、Spec 009 回滚配置或录制格式。

## 五、验收标准

### 5.1 后端 Go 单测（`go test -race -count=1 ./server`）

1. **预设与启动命令构造**：验证 `""` / `"shell"`、`"codex"`、`"agy"` 均合法并产出预期 `cmd` 与 `argv`（`codex --yolo` 与 `agy --dangerously-skip-permissions` 走 `shell -lc 'exec "$@"'`），未知 `preset` 返回 `400`；验证 `cwd` 仅通过 `cmd.Dir` 传递而非拼进 Shell 命令行。
2. **`cwd` 校验与规范化**：空/空白规范为 `$HOME`、`~` 与 `~/sub` 正确展开并 Clean；相对路径、不存在路径、普通文件路径、含 NUL 字节路径一律 `400`。
3. **自动命名与并发去重**：空 `name` 在项目目录生成 `<dir>-<cmd>`、主目录生成 `<cmd>`，重名依次 `-2`、`-3`；显式指定重名仍返回 `409`。
4. **`.launch.json` 一次性生命周期、重启恢复与 `s.cli == false`**：
   - 验证 `.launch.json` 以 `0600` 创建、worker 启动读取后立即删除、启动失败时 `defer` 清理、损坏 `.launch.json` 使 worker 报错退出；
   - 验证通过 `POST /api/term`（含 `cwd` 与指定命令）真实拉起子进程后，子进程实际工作目录与命令结果符合预期、`s.cli` 为 `false`，且新 `manager` 重建恢复（`recoverWorkers`）可从 `probe` 完整恢复 `name`、`cwd`、`cmd`。
5. **`GET /api/workspaces`**：验证汇总活跃与已结束会话的非主目录 `cwd`、过滤掉已删除目录与 `$HOME`、去重并按最近创建时间倒序、输出 `~/` 缩写形式；同时验证 `GET /api/term` 保持既有数组结构。

### 5.2 前端构建与真实内嵌二进制 Playwright 验收（独立临时 `LIMPET_HOME` 与独立端口）

1. **构建与既有回归基准**：`cd web && pnpm build`（含 `vue-tsc -b`）通过，`go build -tags limpet_embed` 成功；在独立临时目录和端口由 `tests/limet-reconnect-browser-test.mjs` 跑通全部 **10/10 既有场景**；U2 未改 `server/` 时可复用 U1 Go `-race` 基准。
2. **工作区聚合、消歧、折叠与样式**：
   - 创建属于两个不同目录（含一对同名叶目录，如 `a/demo` 与 `b/demo`）和主目录的会话，验证侧边栏按工作区准确分组、同名自动父级消歧、「主目录」在后、点击折叠/刷新后保持记忆、打开/新建自动展开；
   - 验证会话行无重复路径、显示命令微徽章、桌面细指针下操作按钮 hover/focus-within 显露、`#quick-switch` 同步按工作区分组。
3. **顶栏组合「新建」与弹窗交互**：
   - 验证左侧「新建」打开默认 `Shell` 弹窗、右侧下拉展示 `Shell`/`Codex`/`Agy` 并可预选进入弹窗；
   - 验证新建弹窗列出 `GET /api/workspaces` 历史目录标签、可点击填入也可手输路径、提交非法路径内联报错 `400`、成功创建后自动关闭弹窗并直接进入终端。
4. **工作区组头 `+` 秒建与端到端实跑**：
   - 在某工作区组头点 `+` → 选 `Shell`，无确认弹窗直接秒建并进入终端，再点一次自动命名 `-2`；
   - 在隔离测试环境注入临时 `codex`/`agy` 可执行 shim，并验证即使登录环境关联类似别名场景也不触发套娃，端到端真实按 `--yolo` 与 `--dangerously-skip-permissions` 在目标工作目录启动。
5. **移动端 `360px` 布局**：在 `360px` 视口下验证分组列表、组头 `+` 菜单、顶栏组合按钮、触控常显按钮与新建弹窗均无页面横向溢出。
