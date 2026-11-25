# 回归脚本

协议级测试脚本（Node ≥ 22，直接 `node <脚本>` 运行）。它们是「做完了吗」的裁判：新架构
（spec 003）帧协议不变，这些脚本就是重写后的回归基线。

公共环境在 `env.mjs` 里，都可用环境变量覆盖：

- `BASE`：被测 server 的地址，默认 `http://127.0.0.1:4040`。
- `LIMPET_HOME`：被测 server 的数据目录，**必须与它启动时用的一致**（socket 与录制都在这里）。
  测试请对着用 `LIMPET_HOME=~/.limpet-dev` 启动的开发 server 跑，不要对着正式服务的数据目录：

  ```sh
  LIMPET_HOME=~/.limpet-dev limpet-server &          # 开发 server，4040
  LIMPET_HOME=~/.limpet-dev node tests/limet-xxx-test.mjs
  ```

跑完用 `node tests/cleanup.mjs` 收尾（关闭全部会话并等 worker 退出）。

| 脚本 | 验证什么 | 新架构下的状态 |
|---|---|---|
| `limet-proto-test.mjs` | 接入即重建、ready 先于输出、尺寸申报与输入者晋升 | 保留，权威 |
| `limet-bigpaste-test.mjs` | 1.1MB 中文大粘贴，端到端 md5 一致 | 保留，权威 |
| `limet-chunked-paste-test.mjs` | 900KB 按 64KiB 切块粘贴：连接不断、输出完整送达、Ctrl+C 后 Shell 仍可操作（碎块突发的慢消费者误判回归点） | 保留，权威 |
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
| `limet-heartbeat-test.mjs` | 心跳（spec 005）：ping 间隔 20s；只读不应答的客户端约 60s 收 4002，应答者与只输入者不受影响。约 70 秒 | 保留 |
| `limet-launch-test.mjs` | worker 启动方式（spec 005）：`systemd-run` 方式下 worker 在独立单元里、环境被转交、关闭与 `kill -9` 后单元不残留。其他启动方式自动跳过 | 保留 |
| `limet-service-test.mjs` | 服务方式（spec 005）：server 作为 systemd 用户单元运行时，stop / restart / 被 kill -9 之后会话存活、Shell PID 不变、worker 不在 server 的控制组里、服务回来后可接回。会停启被测单元——只对临时镜像单元跑（`UNIT=limpet-verify BASE=… LIMPET_HOME=…`） | 保留 |
| `limet-reconnect-browser-test.mjs` | 前端自动重连（spec 005），用 Playwright 驱动真实 Chromium：server 重启 / 连接切断 / 登录失效 / 无声失联后自动接回，三种结束路径进入「会话已结束」。约 2 分钟；需要本机有 Playwright + Chromium，且 4040 与 4042 空闲（脚本自己起 server） | 保留 |
| `limet-u3-lifecycle-test.mjs` 的第 2 节 | 多视图语义（spec 004）：第二个接入成功且画面同步（409 已废止） | 已按 004 修订 |

以下脚本随 tmux 退役（它们的 oracle 是 tmux 或旧启动流），不再维护，
需要时从 git 历史翻：tmux-test、prefix-test、handoff、stall、stall2、race、
race-width、failcap、exit-spawnfail、restart1/2。
