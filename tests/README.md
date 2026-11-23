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

以下脚本随 tmux 退役（它们的 oracle 是 tmux 或旧启动流），不再维护，
需要时从 git 历史翻：tmux-test、prefix-test、handoff、stall、stall2、race、
race-width、failcap、exit-spawnfail、restart1/2。
