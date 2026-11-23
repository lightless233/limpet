# Plan 003：自研会话宿主（libghostty worker），退役 tmux

- 日期：2026-11-23
- 对应 Spec：`docs/specs/003-2026-11-23-libghostty-worker-host.md`
- 执行方式：INLINE，单人单会话
- 依赖顺序：U0 与 U1 可并行启动；U2 依赖 U0 + U1；U3、U4 依赖 U1；U5 依赖 U1–U4；U6 收尾

## 单元 0：spike（门槛单元，timebox 一天内）

**问题**：spec 的回放机制留有三个未决问题，不定案 U2 无法开工。

**改动**：最小 Go 程序验证三件事——① go-libghostty 绑定可用（喂字节、解析、无 panic）；② 网格/scrollback 读取的 API 形状（cell 级样式是否可读，还是仅纯文本提取）；③ 快照路径判定：libghostty snapshot encode 的产物能否被客户端 WASM（@wterm/ghostty）restore。

**产出**：机制决策记录，回填 spec「回放与画面重建」一节；若 ①② 不成立，fallback 选项（自写 C shim / 纯 Go VT parser）与降级影响一并报告用户后再继续。

## 单元 1：worker 骨架（不依赖 libghostty）

**问题**：会话宿主是外部 tmux 进程，寻址与生命周期耦合是全部竞态的根源。

**改动**：
- 同一二进制新增 `worker <uuid>` 子命令：打开 `~/.limpet/workers/<uuid>.sock`（0700）、等 server 的启动指令（携带定案尺寸——首条尺寸/3s 兜底/query 兜底仍在 server WS 层，worker 不计时）后启动 PTY（`$SHELL`、交互模式、进程组、起始目录 `$HOME`、`TERM` 显式设置）、raw tee 录制到 `~/.limpet/history/<uuid>.log`、字节透传不缓冲。
- socket 协议（内部私有，**带版本握手**——server 升级后必须兼容运行中的旧版本 worker，加法演进）：attach / input / resize / kill / exit 通知；单消费者，第二个消费者报 busy。
- server 侧：会话表切换为 uuid → worker 句柄；`POST /api/term` 自 spawn worker；WS handler 桥接 socket；404/409/426/1008/1011/1000 语义逐项保持。
- 回放暂用 raw 尾部（**已知 alt-screen 降级，vim/颜色验收挂起到 U2**）。

**完成证据**：uuid 化的帧级回归脚本全绿（vim 接续与颜色滚动两项除外，它们在 U2 恢复）；1.1MB 中文粘贴 md5 保真；1008/ready 时序不变。

## 单元 2：画面重建（依赖 U0 结论）

**改动**：按定案机制实现——快照 restore 路径（worker encode → 客户端 restore）或网格序列化路径（cell 级读取 → 「网格 → 转义序列」序列化器，onlcr/转义边界处理沿用 001 回放管线的经验）。同时落地：client 流 `smcup`/`rmcup` 过滤（worker 网格吃原始流）、scrollback 容量与回放深度对齐（10000 行 + 余量）、resize 触发网格重排与 PTY 同步。

**完成证据**：vim 接续原样恢复（含跨 server 重启）；长输出重连直接滚动可见且含颜色；重接字节量符合所选机制的预期（快照 O(屏+尾) / 序列化与 capture 等价抽查比对）。

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

**改动**：按 spec 验收 1–14 逐条执行；移动视口项以 DevTools 仿真自测，真机清单（换设备接续、按键栏、软键盘）留用户。

**完成证据**：自动化全绿 + 真机清单；001 验收 1–12 的行为等价性逐条比对记录。

## 风险与备注

- libghostty scrollback/reflow 子 API 未冻结：版本 pin + 升级纳入回归；spike 若证明绑定不可用，动工前必须回到用户做 fallback 决策。
- Shell 环境差异：tmux 注入的 `TERM`/环境消失，worker 显式设置——首屏行为是重点回归观察点（% 提示符一类问题若复现，优先查 `TERM` 与首屏重绘路径）。
- 每会话一进程：worker 网格 scrollback 与回放深度对齐（~10k 行），内存低于 tmux 时代（50k 行）；进程数个人规模（几十）不构成问题。
- 一致性窗口：worker 死亡到列表观测之间——「首次观测死亡」语义，与 002 注册表设计一致。
- 过渡期风险：U1 完成、U2 未完成时 vim/颜色保真暂缺——不部署、不切主线流量，U2 完成前 tmux 宿主保持可用（切换点 = U5）。
