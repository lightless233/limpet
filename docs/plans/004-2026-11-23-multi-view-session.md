# Plan 004：会话多视图（N 渲染器 + 尺寸跟随输入者）

- 日期：2026-11-23
- 对应 Spec：`docs/specs/004-2026-11-23-multi-view-session.md`
- 执行方式：INLINE，单人单会话
- 依赖顺序：U1 → U2 → U3 → U4；U1 与 U2 同文件耦合紧，顺序做

## 单元 1：worker 广播枢纽

**结果（2026-11-23）**：完成——viewer 集合 + per-viewer 队列/切点/申报尺寸、输入晋升（尺寸相同跳过）、接入即重建；全部旧单槽代码删除。

**问题**：worker 的消费者是单槽（CAS + 409），同一会话第二个接入被拒。

**改动**：
- `consumer` 槽位改为 **viewer 集合**（map[conn]*viewer + 互斥；viewer = 连接、有界队列、申报尺寸、cutOffset、closed）。
- attach 握手：不做独占判定，登记新 viewer 即可；接入即重建（formatter 按网格当前尺寸 + 实时尾巴——不再等 resize，3s 兜底与 query 兜底删除）。
- 广播：`handleOutput` 输出批次逐 viewer 入队（切点后字节）；队列打满仅丢弃该 viewer（DETACH 4001 → conn 关闭），会话与其他 viewer 不受影响。
- 输入与晋升：INPUT 帧 → 若发送者申报尺寸 ≠ 网格尺寸 → resize（每批次至多一次）；字节写 PTY。
- 协议：worker 侧 `protocolVersion` 递增为 2；ftResize 语义改为「窗口尺寸申报」（存 viewer 申报值，不直接触发 resize）；重建由 attach 触发。
- 慢消费者、DETACH(4001)、影子网格、录制、查询应答路由：逻辑原样保留（路由按各 viewer 的切点）。

**完成证据**：单 worker 上两个连接同时 attach 均就绪；任一连接输入在两个连接的输出流中可见；高吞吐下一连接暂停读取被 4001 丢弃，另一连接不受影响。

## 单元 2：版本适配（升级窗口）

**结果（2026-11-23）**：完成——worker protocolVersion=2，握手对 ≤2 向下兼容；server 的 probe/kill/attach 带 2→1 版本回退（升级窗口内的旧 worker 实测可适配）。

**问题**：ftResize 语义变更后，升级窗口内的旧 worker（v1：resize 即尺寸、重建等 resize）与新 server 混跑。

**改动**：握手应答已带 worker 版本——server 按版本分派：
- v2 worker：申报帧原样转发（worker 自己处理晋升）。
- v1 worker：接入时以接入者申报尺寸**合成一次 ftResize**（触发其重建，沿用旧时序）；后续申报照发（旧 worker 的「resize 跟随申报」行为）。
- 新 worker 对 v1 server 不做适配要求（同二进制升级，worker 短生命周期）。

**完成证据**：升级重启场景（新 server × 旧 worker）接入可用、重建正常（spec 003 验收 9 的升级部分继续成立）。

## 单元 3：server bridge 多中继

**结果（2026-11-23）**：完成——busy 预检与 409 删除，同一会话多条 WS 中继并存，探测 Busy 正确反映接入数。

**问题**：bridge 的 busy 预检与 409、handle 的单 relay 假设。

**改动**：删除 busy 预检与 409 分支；同一会话的多条 WS 各自独立中继（每条一条 socket 连接）；list 的 `attached` = viewer 数 > 0（来自探测应答的 Busy）；h.busy 每次探测后更新。

**完成证据**：两个 WS 同时中继无串扰；探测 Busy 正确反映接入数。

## 单元 4：验收与脚本

**结果（2026-11-23）**：完成——limet-multi-view-test.mjs 12 项断言全过（双 viewer 输入互通、慢 viewer 单独 4001、断开重连恰好一次），u3-lifecycle 切多视图语义，全套 11 脚本回归绿。

**改动**：
- 新脚本 `limet-multi-view-test.mjs`：双 WS 同会话（均 ready / 输入互通 / 尺寸跟随输入者）/ 慢 viewer 4001 隔离 / 重连恰好一次。
- `limet-u3-lifecycle-test.mjs`：409 断言废止，改为多视图断言（第二个接入成功且两路输出一致）。
- 其余回归脚本与单测全跑；`-race` 全套。
- 前端零改动验证：现有 H5 双窗口流程（DevTools 375）。

**完成证据**：spec 004 验收 1–9 全过；003 回归 11 脚本全绿（u3-lifecycle 按新语义修订后）；`-race` 构建下多视图/慢消费者/生命周期三脚本零告警。

## 风险与备注

- 广播路径的锁粒度：viewer 集合互斥与网格互斥分离，广播入队在网格锁外（批次快照后逐 viewer 入队，不阻塞 PTY 读取——003 背压原则延续）。
- 查询应答重复（多 viewer 已知限制）：出问题时的现象是提示符出现少量垃圾字符，不影响会话状态；去重留 v2。
- 观察者折行（尺寸不一致）是语义的一部分，前端不需要任何处理；将来「完美 per-viewer 渲染」为独立增强（RenderState 脏行 → VT）。
- spec 003 U6 hold：多视图落地后，003/004 合并执行最终验收（浏览器视觉 + 真机），避免重复验收。
