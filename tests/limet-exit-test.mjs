// exit 后连接应主动断开（前端才会显示已断开并提供重连）。
const { BASE, WS } = await import('./env.mjs')
const enc = new TextEncoder()
const dec = new TextDecoder()
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const res = await fetch(`${BASE}/api/term`, { method: 'POST' })
const { id } = await res.json()
const ws = new WebSocket(`${WS}/api/ws/term/${id}`)
ws.binaryType = 'arraybuffer'
let out = ''
let closed = false
let closeAt = 0
const t0 = performance.now()
ws.onclose = () => { closed = true; closeAt = Math.round(performance.now() - t0) }
ws.onmessage = (e) => { out += (typeof e.data === 'string' ? '' : dec.decode(e.data)) }
await new Promise((r) => { ws.onopen = r })

ws.send(enc.encode('exit\r'))
// 给足 3 秒观察
for (let i = 0; i < 6 && !closed; i++) await sleep(500)
console.log(`exit 后: closed=${closed}${closed ? ` (${closeAt}ms 内)` : ' (3s 仍未断开)'} 输出 ${out.length}B`)
process.exit(closed ? 0 : 1)