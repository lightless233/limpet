# Plan 003：自研会话宿主（libghostty worker），退役 tmux

- 日期：2026-11-23
- 对应 Spec：`docs/specs/003-2026-11-23-libghostty-worker-host.md`
- 执行方式：INLINE，单人单会话
- 依赖顺序：U0 与 U1 可并行启动；U2 依赖 U0 + U1；U3、U4 依赖 U1；U5 依赖 U1–U4；U6 收尾

## 单元 0：spike（门槛单元，timebox 一天内）

**问题**：重建链路的最后一环是浏览器端 `@wterm/ghostty` WASM 吃下 VT 序列之后的实际渲染与滚动缓冲。单测能读出网格和样式，证明不了这条链路。另有四件事只能靠实证定案。

**前置**：本机当前未安装 Zig 与 libghostty-vt（gcc、pkg-config 已有）。安装 Zig 0.17.x，按官方 `PACKAGING-LIBGHOSTTY.md` 构建 `vt-1.0.1`：下载同版本的源码包与依赖包并排解包，`zig build --prefix /usr --system ../libghostty-vt-1.0.1-deps -Doptimize=ReleaseFast -Dcpu=baseline`，`ZIG_GLOBAL_CACHE_DIR` 须指向新的空目录（Zig 0.17 的缓存缺陷会让构建崩溃）；不需要 CMake。Go 绑定 `go.mitchellh.com/libghostty` 以当前伪版本 pin 入 `go.mod`。

**改动**：最小 worker 原型（PTY + 网格 + 重建）对接**现有 H5 页面，不改前端**，依次验证：

1. go-libghostty 绑定可用：喂字节、解析、无 panic。
2. 官方 formatter 的 VT 输出与 extras 的实际行为——活动屏、样式、光标。
3. **滚动历史序列化**：经 `ScrollbackRows` / grid 遍历读出含样式的行并编码为 VT 序列。formatter 没有这项能力（只格式化 active screen，无 scrollback 选项），这一段自建，是链路中风险最高的一段。
4. **真实浏览器三项**：彩色滚动历史、全屏画面（vim 中断线后重接）、换尺寸后继续操作。
5. 切点对齐原型：在持续输出的会话上重接，重建帧与实时流衔接处无重复、无丢失；帧序保持 ready → 重建帧 → 实时流。
6. **全屏程序运行中重接**：能否读出非活动（主）屏的内容与光标；重建后退出全屏，画面是否与从未断线时一致。
7. **查询应答仲裁**：安装写回回调后，离线时 worker 代答、在线时丢弃并交由浏览器；同一查询在线恰好一次；两端应答内容是否一致；需同步应答的回调（如剪贴板读取）在线时如何避免双答。

**产出**：机制决策记录，回填 spec「回放与画面重建」。第 4、6 项不过则 U2 不得开工——替代路径（raw 尾部回放等）与降级影响先报告用户再继续；第 1–3 项不成立时同理，fallback（自写 C shim / 纯 Go VT parser）先报告再继续。

## 单元 1：worker 骨架（不依赖 libghostty）

**问题**：会话宿主是外部 tmux 进程，寻址与生命周期耦合是全部竞态的根源。

**改动**：
- 同一二进制新增 `worker <uuid>` 子命令：打开 `~/.limpet/workers/<uuid>.sock`（0700）、等 server 的启动指令（携带定案尺寸——首条尺寸/3s 兜底/query 兜底仍在 server WS 层，worker 不计时）后启动 PTY（`$SHELL`、交互模式、进程组、起始目录 `$HOME`、`TERM` 显式设置）、raw tee 录制到 `~/.limpet/history/<uuid>.log`、字节透传不缓冲。
- socket 协议（内部私有，**带版本握手**——server 升级后必须兼容运行中的旧版本 worker，加法演进）：attach / input / resize / kill / exit 通知；单消费者，第二个消费者报 busy。
- server 侧：会话表切换为 uuid → worker 句柄；`POST /api/term` 自 spawn worker；WS handler 桥接 socket；404/409/426/1008/1011/1000 语义逐项保持。
- 发往浏览器的流不做任何过滤（备用屏采用标准行为）。回放暂用 raw 尾部（**已知 alt-screen 降级，vim/颜色验收挂起到 U2**）。切点对齐从本单元起就必须成立：尾部回放取到输出位置 X，实时流从 X 之后接，回放生成期间到达的输出先缓冲。

**完成证据**：uuid 化的帧级回归脚本全绿（vim 接续与颜色滚动两项除外，它们在 U2 恢复）；1.1MB 中文粘贴 md5 保真；1008/ready 时序不变。

## 单元 2：画面重建（依赖 U0 结论）

**改动**：按 U0 定案实现 VT 重建——滚动历史段（含样式的行 → VT 序列，自建）+ 活动屏段（官方 formatter 的 VT 输出 + 选定 extras），经现有二进制帧进入 wterm 的 `write` 入口，前端不改。落实切点对齐：重建期间缓冲 PTY 输出，重建帧发完后按序放行；帧序 ready → 重建帧 → 实时流。重接先按客户端首条尺寸重排网格与 PTY，再生成重建画面。同时落地：全屏程序运行中重接的重建（主屏内容段 → 进入备用屏 → 备用屏内容段）、终端查询应答仲裁（写回回调：离线写入 PTY，在线丢弃）、scrollback 容量与回放深度对齐（10000 行 + 余量）。

**完成证据**：真实浏览器四项通过——彩色滚动历史、全屏画面（vim 中断线后重接，含跨 server 重启；退出 vim 后还原为进入前的主屏、无残迹）、换尺寸后继续操作、终端查询应答（离线有应答，在线恰好一次）；切点对齐断言通过（带序号的输出流，重接后每行恰好出现一次）；帧序为 ready → 重建帧 → 实时流。

## 单元 3：生命周期加固

**改动**：
- server 启动扫描 `~/.limpet/workers/`：stale socket（进程已死）清理，活 worker 懒连接恢复；升级场景（新二进制 server × 旧版本 worker）的协议版本握手验证。
- close 路径：TERM → 兜底 KILL 的进程组终止升级、socket 移除、僵尸回收。
- exit 路径：shell 退出 → 消费者收 1000、socket 清理、注册表 `endedAt` 首次观测登记。

**完成证据**：spec 验收 9/10/5/6——server `kill -9` 重启后会话全存活且录制不中断；worker `kill -9` 单会话死亡、其余不受影响、stale socket 清理。

## 单元 4：注册表与 API/前端切换

**改动**：
- `~/.limpet/sessions.json`：原子写、单写者互斥；创建登记、死亡登记、改名更新。
- API 全面 uuid 化：创建返回体、列表 `{id, name, created, attached}`、`rename`（只改注册表 name，id/地址不变）、`close`；名称唯一性按注册表（409 保留——产品选择，重名已无技术必要性），校验规则沿用。
- 前端：列表字段、WS URL、close/rename 调用点全部切 uuid；行为零变化。

**完成证据**：改名后 id 与接入地址不变；重名 409；同名再建 uuid 不同；前端全流程（DevTools 375）无回归。

## 单元 5：tmux 退役清理

**改动**：
- 删除 `server/tmux.go` 全部隔离层（socket `-L limpet`、托管配置、`LIMPET_TMUX_BIN`、精确寻址、pipe/capture 封装）及 server 内全部 tmux 调用点。
- 测试 oracle 改造：`tmux list-clients` 断言 → worker socket 自省/进程枚举；现有脚本（tmux 套件、回放、粘贴、前缀、交接、stall、race）uuid 化后重跑，前缀寻址类测试随 tmux 寻址消失而退役。

**完成证据**：`grep -ri tmux server/ web/` 无运行时调用残留；全回归（含 `-race`）通过；spec 验收 14 成立。

## 单元 6：验收整合

**改动**：按 spec 验收 1–17 逐条执行；验收 3/4/15/16/17 以真实浏览器为准（现有 H5 + `@wterm/ghostty` 0.6.0），协议级脚本只作为它们的字节序补充；移动视口项以 DevTools 仿真自测，真机清单（换设备接续、按键栏、软键盘）留用户。

**完成证据**：自动化全绿 + 真机清单；001 验收 1–12 的行为等价性逐条比对记录。

## 风险与备注

- libghostty 与 Go 绑定均需 pin 版本（库 ABI 自 1.0 稳定，绑定仍是无正式 tag 的伪版本）：升级纳入回归；spike 若证明绑定不可用，动工前必须回到用户做 fallback 决策。
- 重建链路的最高风险项是滚动历史段的序列化——官方 formatter 只覆盖活动屏，这一段自建，且只有真实浏览器能证明它成立。U0 第 4 项是整条路线的门槛。
- 备用屏采用标准行为后，全屏程序运行中重接要求读出非活动屏的内容与光标。库区分两块屏幕，但能否读取未实证——U0 第 6 项，读不到即无法满足保真合同。
- 本机工具链前置：Zig 与 libghostty-vt 均未安装，U0 开工第一步是搭起来；若 libghostty-vt 在本机构建受阻，先报告再决定，不绕路。
- Shell 环境差异：tmux 注入的 `TERM`/环境消失，worker 显式设置——首屏行为是重点回归观察点（% 提示符一类问题若复现，优先查 `TERM` 与首屏重绘路径）。
- 每会话一进程：worker 网格 scrollback 与回放深度对齐（~10k 行），内存低于 tmux 时代（50k 行）；进程数个人规模（几十）不构成问题。
- 一致性窗口：worker 死亡到列表观测之间——「首次观测死亡」语义，与 002 注册表设计一致。
- 过渡期风险：U1 完成、U2 未完成时 vim/颜色保真暂缺——不部署、不切主线流量，U2 完成前 tmux 宿主保持可用（切换点 = U5）。
