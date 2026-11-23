// 按前端契约分块发送 900KB（64KiB/条）：连接不断、输出完整送达、Ctrl+C 之后 Shell 仍可操作。
//
// 回归点（spec 005 施工中发现的原有缺陷）：Shell 逐段回显长行时 PTY 每批只有几十字节，
// viewer 队列曾按块计数——256 个 27 字节的碎块就被当成慢消费者，连接在几十毫秒内被断开
// （1011/4001），或因为丢弃时等写锁而让 PTY 读取停摆。这个脚本此前只打印不判定，没抓到。
const { BASE, WS } = await import('./env.mjs')
const enc = new TextEncoder()
const dec = new TextDecoder()
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const CHUNK = 64 * 1024
let pass = true
const has = (n, ok, d) => {
  console.log(`  [${ok ? '通过' : '失败'}] ${n}${ok ? '' : ' —— ' + (d ?? '')}`)
  if (!ok) pass = false
}

const { id } = await (await fetch(`${BASE}/api/term`, { method: 'POST' })).json()
const ws = new WebSocket(`${WS}/api/ws/term/${id}`)
ws.binaryType = 'arraybuffer'
let out = ''
let bytes = 0
let closed = 0
ws.onclose = (e) => { closed = e.code || 1006 }
ws.onmessage = (e) => { if (typeof e.data !== 'string') { bytes += e.data.byteLength; out += dec.decode(e.data) } }
await new Promise((r) => { ws.onopen = r })

// 模拟大段粘贴：900KB 按 64KiB 分块，等价于前端 sendInput 行为。
const payload = enc.encode('echo ' + 'C'.repeat(900000) + '\r')
for (let i = 0; i < payload.length; i += CHUNK) {
  ws.send(payload.slice(i, i + CHUNK))
}
await sleep(800)
has('分块发送后连接未断开', !closed, `关闭码 ${closed}`)

// Ctrl+C 中断 Shell 对超长行的消化，然后确认它还听使唤。
ws.send(enc.encode('\x03'))
await sleep(400)
const marker = 'limpet-alive-after-chunked-paste'
ws.send(enc.encode(`echo ${marker}\r`))
const t0 = Date.now()
while (!out.includes(marker) && !closed && Date.now() - t0 < 10000) await sleep(50)
has(`输出完整送达（${(bytes / 1048576).toFixed(2)} MiB）`, bytes >= 800000)
has(`Ctrl+C 之后 Shell 仍可操作（${((Date.now() - t0) / 1000).toFixed(1)}s）`, out.includes(marker))
has('全程连接未断开', !closed, `关闭码 ${closed}`)

try { ws.close() } catch {}
await fetch(`${BASE}/api/term/${id}/close`, { method: 'POST' })
console.log(pass ? '=> 通过' : '=> 失败')
process.exit(pass ? 0 : 1)
