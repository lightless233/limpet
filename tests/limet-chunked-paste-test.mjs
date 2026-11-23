// 按前端契约分块发送 900KB（64KiB/条），中断后检查 Shell 存活。
const enc = new TextEncoder()
const dec = new TextDecoder()
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const CHUNK = 64 * 1024

const res = await fetch('http://127.0.0.1:4040/api/term', { method: 'POST' })
const { id } = await res.json()
const ws = new WebSocket(`ws://127.0.0.1:4040/api/ws/term/${id}`)
ws.binaryType = 'arraybuffer'
let out = ''
let closed = false
ws.onclose = () => { closed = true }
ws.onmessage = (e) => { out += (typeof e.data === 'string' ? '' : dec.decode(e.data)) }
await new Promise((r) => { ws.onopen = r })

// 模拟大段粘贴：900KB 按 64KiB 分块，等价于前端 sendInput 行为
const payload = enc.encode('echo ' + 'C'.repeat(900000) + '\r')
for (let i = 0; i < payload.length; i += CHUNK) {
  ws.send(payload.slice(i, i + CHUNK))
}
await sleep(800)
console.log(`分块发送后: closed=${closed}`)
// Ctrl+C 中断 zsh 对超长行的消化，避免重绘风暴拖时间
ws.send(enc.encode('\x03'))
await sleep(400)
ws.send(enc.encode('echo limpet-alive-after-chunked-paste\r'))
await sleep(600)
console.log(`closed=${closed} 存活检查=${out.includes('limpet-alive-after-chunked-paste') ? '✓' : '✗'}`)
process.exit(0)
