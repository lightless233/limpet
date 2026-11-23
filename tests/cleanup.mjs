// 清理工具：关闭 BASE 上的全部活会话并等 worker 进程退出。
// 用法：node tests/cleanup.mjs      LIMPET_HOME 需与被测 server 一致（见 env.mjs）
import { execSync } from 'node:child_process'
const { BASE } = await import('./env.mjs')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// 不依赖二进制叫什么名字（开发构建 / 正式安装的名字不同），只认命令行形态。
const workers = () =>
  parseInt(execSync(`pgrep -fc -- '[l]impet[^ ]*server worker [0-9a-f]{16}$' || true`).toString().trim() || '0', 10)

const list = await (await fetch(`${BASE}/api/term`)).json()
console.log(`关闭 ${list.length} 个会话…`)
for (const s of list) {
  await fetch(`${BASE}/api/term/${s.id}/close`, { method: 'POST' })
}
// 等全部 worker 退出（关闭路径带 TERM→KILL 宽限）
const t0 = Date.now()
while (Date.now() - t0 < 15000 && workers() > 0) await sleep(300)
const left = workers()
console.log(`遗留 worker 进程: ${left}`)
process.exit(left === 0 ? 0 : 1)
