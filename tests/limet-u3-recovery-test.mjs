// U3 重启恢复验证（spec 003 验收 9/10/18）：给定重启前创建的会话 id，断言——
// 列表恢复（含名字）→ 接入 ready → 重启前的标记在重建画面里 → 重启后输出可用。
// 用法：node tests/limet-u3-recovery-test.mjs <session-id>
const { BASE, WS } = await import('./env.mjs')
import { execSync } from 'node:child_process'
const dec = new TextDecoder()
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const id = process.argv[2]
if (!id) {
  console.error('用法: node limet-u3-recovery-test.mjs <session-id>')
  process.exit(2)
}

let pass = true
const has = (name, ok) => {
  console.log(`  [${ok ? '通过' : '失败'}] ${name}`)
  if (!ok) pass = false
}

// 1. 列表恢复且含名字
const list = await (await fetch(`${BASE}/api/term`)).json()
const entry = list.find((s) => s.id === id)
has('列表恢复该会话', !!entry)
has('名字保持（注册表）', entry?.name === 'u3-survive')

// 2. 接入：ready → 重建画面含重启前标记
const ws = new WebSocket(`${WS}/api/ws/term/${id}?cols=100&rows=30`)
ws.binaryType = 'arraybuffer'
let ready = false
let text = ''
ws.onmessage = (e) => {
  if (typeof e.data === 'string') {
    try {
      if (JSON.parse(e.data).type === 'ready') ready = true
    } catch {}
    return
  }
  text += (typeof e.data === 'string' ? '' : dec.decode(e.data))
}
await new Promise((r) => (ws.onopen = r))
const t0 = Date.now()
while (!ready && Date.now() - t0 < 10000) await sleep(50)
has('接入 ready', ready)
await sleep(1200) // 让重建帧与实时流都进来
has('重建画面含重启前标记（U3-MARKER-开始）', text.includes('U3-MARKER-开始'))

// 3. 重启后输出可用
ws.send(Buffer.from('echo U3-重启后输出OK\r'))
await sleep(1200)
has('重启后输出可用（U3-重启后输出OK）', text.includes('U3-重启后输出OK'))

ws.send(Buffer.from('exit\r'))
await sleep(600)

if (!pass) {
  console.log('=> 失败')
  process.exit(1)
}
console.log('=> 通过')
process.exit(0)