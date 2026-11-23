# 回归脚本

协议级测试脚本（Node ≥ 22，直接 `node <脚本>` 运行，默认 BASE=`http://127.0.0.1:4040`，
可用环境变量 `BASE` 覆盖）。它们是「做完了吗」的裁判：新架构（spec 003）帧协议不变，
这些脚本就是重写后的回归基线。

| 脚本 | 验证什么 | 新架构下的状态 |
|---|---|---|
| `limet-proto-test.mjs` | ready 先于输出、ready 前输入 1008、尺寸消息时序 | 保留，权威 |
| `limet-bigpaste-test.mjs` | 1.1MB 中文大粘贴，端到端 md5 一致 | 保留，权威 |
| `limet-chunked-paste-test.mjs` | 64KiB 切块的粘贴 | 保留，权威 |
| `limet-paste-test.mjs` | 一般粘贴回显 | 保留 |
| `limet-exit-test.mjs` | Shell 内 exit → 会话结束、连接关闭（1000） | 保留 |
| `limet-lifecycle-test.mjs` | 创建/列表/关闭的语义 | 保留（会话 id 变为 uuid） |
| `limet-size-probe.mjs` | 首屏尺寸行为（首条尺寸生效） | 保留，观察用 |
| `limet-replay-test.mjs` | 重连后收到断开前产生的历史行 | 保留（机制从 capture 回放换为 VT 重建，外部行为不变） |
| `limet-alt-reconnect-test.mjs` | 备用屏中重连：影子主屏还原 + 顺序正确 + 退出后会话可用 | 保留 |
| `limet-u3-lifecycle-test.mjs` | 关闭回收整棵进程树（前台+后台作业）；第二个接入升级前 409 | 保留 |
| `limet-u3-recovery-test.mjs` | server 重启后的会话恢复断言（配合外部重启编排使用，见验收 9 三场景） | 保留 |
| `cleanup.mjs` | 关闭全部会话并等 worker 进程退出（跑完套件后收尾） | 保留 |
| `limet-multi-view-test.mjs` | 多视图（spec 004）：双 viewer 输入互通、慢 viewer 单独丢弃 4001、断开重连恰好一次 | 保留 |
| `limet-u3-slow-consumer-test.mjs` | 慢消费者：30MB 负载 + 停读占空比，连接被关闭（4001 帧或 TCP 关闭）且会话/他人不受影响 | 保留 |
| `limet-u3-lifecycle-test.mjs` 的第 2 节 | 多视图语义（spec 004）：第二个接入成功且画面同步（409 已废止） | 已按 004 修订 |

以下脚本随 tmux 退役（它们的 oracle 是 tmux 或旧启动流），不再维护，
需要时从 git 历史翻：tmux-test、prefix-test、handoff、stall、stall2、race、
race-width、failcap、exit-spawnfail、restart1/2。
