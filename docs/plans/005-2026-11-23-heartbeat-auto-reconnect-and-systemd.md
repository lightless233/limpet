# Plan 005：心跳、前端自动重连与 systemd 部署

- 日期：2026-11-23
- 对应 Spec：`docs/specs/005-2026-11-23-heartbeat-auto-reconnect-and-systemd.md`
- 执行方式：INLINE，单人单会话
- 依赖顺序：U1 → U2（前端看门狗依赖 server 的 ping）；U3 → U4（服务单元依赖 systemd-run 启动方式）；两条线互不依赖；U5 收尾。与用户所列顺序一致：心跳 → 自动重连 → systemd。

## 单元 1：心跳（server ↔ 浏览器）

**结果（2026-11-23）**：完成——relay 内每条连接一个心跳定时器（20s 发 ping，60s 没听到对端以 4002 关闭），入站的任何消息都算听到对端；ping/pong 不进 worker。`limet-heartbeat-test.mjs` 12 项断言全过：不应答的客户端在 60.0s 收到 4002，应答 pong 的与只输入不回 pong 的 viewer 都不受影响，ping 间隔 20.0s，全部 viewer 退出后「已接入」解除。`-race` 构建下心跳 / 多视图 / 慢消费者三脚本零告警。

**问题**：WS 上只有终端数据，没有保活；server 发现不了无声死亡的对端。

**改动**：
- relay 的 WS 读侧识别 `{"type":"pong"}`（现只识别 resize，其余文本帧丢弃），刷新「最近听到对端」的时刻；输入与尺寸申报同样算听到。
- 每条连接一个 20 秒定时器：发 `{"type":"ping"}`（复用现有 WS 写互斥与 5 秒写超时）；连续 60 秒没听到对端 → 以 4002 关闭。ping/pong 不经 socket 进 worker。
- 新脚本 `limet-heartbeat-test.mjs`：ping 节律（20s ± 1s）；只读不应答的客户端约 60 秒收 4002，随后该会话「已接入」解除、另一个应答 pong 的 viewer 不受影响；应答 pong 的连接跨两个周期仍存活。

**完成证据**：脚本通过；`-race` 干净。现有脚本均短于 60 秒且已忽略非 ready 文本帧，不需改动。

## 单元 2：前端自动重连

**问题**：断线后只有手动按钮；1000 / 4001 / 1006 提示相同；连接无声死亡时页面停在「已连接」。

**改动**（`TerminalView.vue`）：
- 状态改为：接入中 / 重连中 / 已连接 / 会话已结束 / 登录已失效 / 错误，徽标文案随状态。
- 收到 ping 回 pong；收到任何消息刷新「最近收到」时刻；60 秒没收到 → 关闭当前连接并走重连。
- 关闭处理：1000 → 会话已结束；其余关闭码与握手失败 → 用 `GET /api/term` 核对（在 → 重连；不在 → 会话已结束；被重定向 → 登录已失效；请求失败 → 重连）。1011 并入这条路径，「终端启动失败」的特例删除。
- 退避 1s 起倍增、封顶 30s，ready 复位；`online` 与页面重新可见时未连上则立即尝试。手动「重新连接」按钮只留在错误状态。
- 卸载时摘掉事件监听与定时器；现有「旧连接的回调不污染新连接」的语义不变。
- 失联模拟工具：一个只做字节转发的 TCP 中继（挂在 vite 前面），在 WebSocket 升级完成后丢弃 server → 浏览器方向的字节，用来验证浏览器侧的判定。

**完成证据**（浏览器）：DevTools Offline → Online 自动接回，画面为当前屏且输出恰好一次；中继单向停滞后 60 秒内自动重连；`exit` 显示「会话已结束」且不再重连；server 停止再启动后页面自动接回。

## 单元 3：worker 经 systemd-run 启动

**问题**：server 运行在服务单元内时，直接 spawn 的 worker 留在同一控制组，server 单元停止或重启时被 systemd 一并清理。

**改动**：
- 配置项 `LIMPET_WORKER_LAUNCH`（`direct` 默认 / `systemd-run`）。
- `systemd-run` 方式的创建路径：`systemd-run --user` 拉起 `limpet-worker-<uuid>.service`（`--collect`、exec 类型，worker 日志仍落 `~/.limpet/workers/<uuid>.log`），把 server 自己的环境显式转交给单元（剔除 systemd 给 server 单元专用的几个变量）；之后沿用现有的「轮询 socket + uuid 核对」就绪判定，以及全部关闭与恢复路径。
- 启动保护：`systemd-run` 方式先做一次最小单元拉起验证，失败即拒绝启动；`direct` 方式发现自身在服务单元内时告警。
- `systemd-run` 不可用或拉起失败 → 创建请求失败，不留半死会话（与现有语义一致）。

**完成证据**：`systemd-run` 方式下创建会话成功，worker 的控制组为 `limpet-worker-<uuid>.service`，环境与 server 一致；`direct` 方式回归不受影响。单元文件在 U4 落地，本单元先用等价环境验证。

## 单元 4：发布构建、安装目录与服务单元

**问题**：没有单元文件；发布产物不完整（页面在二进制之外、靠工作目录去找）；构建依赖一个不在 pkg-config 默认搜索路径里的 `.pc` 文件（构建缓存一失效，`go build` 就失败）；安装与升级步骤不成文；开发仓库与正式运行之间没有边界。

**改动**：
- **前端内嵌**：`go.mod` 上移到仓库根（`go:embed` 只能引用模块内的路径）；`web/` 增加内嵌包，只在 `-tags limpet_embed` 的发布构建里编译，开发构建里为空。server 托管页面时内嵌优先，开发构建退回磁盘上的 `web/dist`（现有行为）；文件名带内容哈希的 `assets/` 允许长期缓存，`index.html` 不缓存。
- **版本标记**：发布构建写入提交号；`limpet-server version` 可查，启动日志带版本。
- `deploy/install.sh`：构建前端 → 发布构建（固定携带 libghostty 的 pkg-config 路径）→ 安装目录（默认 `/opt/limpet`；不可写时报错并给出创建命令）→ 生成单元文件（WSL 会话基线取自当前会话，Windows 目录取自当前 PATH，端口 13000）→ 链接进 `~/.config/systemd/user/` → `daemon-reload`、启用、重启。二进制与单元文件都先写临时文件再改名。
- `deploy/README.md`：安装、升级、启停、日志、刷新 PATH 快照。
- `tests/`：WS 地址从 `BASE` 推导（现有脚本写死 4040）；`cleanup.mjs` 等 worker 退出的判定不再依赖二进制名。

**完成证据**：安装目录里只有二进制与单元文件，单元经链接生效、不引用开发仓库路径（spec 验收 15）；服务在 13000 上页面可用（8）；stop / restart / kill -9 三场景下会话存活且新 server 找回；关闭会话与 worker `kill -9` 后无残留单元与进程；再执行一次安装脚本完成一次升级（9–12）；前台 server 与 vite 与服务并行可用（16）；上线前未登录请求被 Access 重定向（17）。

## 单元 5：验收与收尾

**改动**：
- 全套脚本在 `direct`（4040）与 `systemd-run`（13000）两种方式下各跑一遍；`-race`。
- spec / plan 回填结果；真机清单（spec 验收 7）交付用户。

**完成证据**：spec 005 验收 1–6、8–17 有记录；7 待用户执行。

## 风险与备注

- **操作纪律**：停 server 一律用普通 `kill` 或 `systemctl`。按进程树整棵清理的停止方式（包括 agent 工具的「停止后台任务」）会带走 worker——今天已因此结束过一个空闲会话。服务方式下 worker 的父进程是 systemd，这类工具也追不到它。
- **上线检查**：13000 是公网入口。Limpet 自身不做认证，启用服务前先确认 Tunnel 域名在 Access 之后；确认不了就不启用，先报告。
- 服务方式下 server 日志进 journal；worker 日志位置不变。
- 心跳定时器与输出泵并发写同一条 WS，经现有写互斥串行；ping 写超时按慢消费者处理（4001）。
- 测试每跑一轮新增约 250MB 录制（慢消费者测试各 30MB），不在本工作项范围；`~/.limpet/history` 今天已清空。
