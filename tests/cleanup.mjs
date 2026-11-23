// 清理工具：关闭全部活会话并等 worker 进程退出。
// 用法：node tests/cleanup.mjs
import { execSync } from 'node:child_process'
const BASE = process.env.BASE || 'http://127.0.0.1:4040'
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const list = await (await fetch(`${BASE}/api/term`)).json()
console.log(`关闭 ${list.length} 个会话…`)
for (const s of list) {
  await fetch(`${BASE}/api/term/${s.id}/close`, { method: 'POST' })
}
// 等 worker 全部退出（关闭路径带 TERM→KILL 宽限）
const t0 = Date.now()
while (Date.now() - t0 < 15000) {
  const n = parseInt(execSync(`ps -eo args | grep 'new-server worker' | grep -v grep | wc -l`).toString().trim(), 10)
  if (n === 0) break
  await sleep(300)
}
const left = parseInt(execSync(`ps -eo args | grep 'new-server worker' | grep -v grep | wc -l`).toString().trim(), 10)
console.log(`遗留 worker 进程: ${left}`)
process.exit(left === 0 ? 0 : 1)
