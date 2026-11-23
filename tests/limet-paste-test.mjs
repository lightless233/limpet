// 大段粘贴复现：单条 >32KiB 输入消息 → 旧后端应断连并杀 Shell。
const enc = new TextEncoder()
const dec = new TextDecoder()
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const res = await fetch('http://127.0.0.1:4040/api/term', { method: 'POST' })
const { id } = await res.json()
const ws = new WebSocket(`ws://127.0.0.1:4040/api/ws/term/${id}`)
ws.binaryType = 'arraybuffer'
let out = ''
let closed = false
ws.onclose = () => { closed = true }
ws.onmessage = (e) => { out += dec.decode(e.data) }
await new Promise((r) => { ws.onopen = r })

// 单条 60006 字节消息（echo 一长串 A），超过 32768 上限
ws.send(enc.encode('echo ' + 'A'.repeat(60000) + '\r'))
await sleep(600)
console.log(`大消息发送后: closed=${closed} 收到 ${out.length} 字节, 含AAAA=${out.includes('AAAA')}`)

if (!closed) {
  // Shell 存活检查
  ws.send(enc.encode('echo limpet-shell-alive\r'))
  await sleep(600)
  console.log(`存活检查: closed=${closed}, shell-alive=${out.includes('limpet-shell-alive')}`)
}
console.log(closed ? '=> 复现：连接断开 ✗' : '=> 连接存活 ✓')
process.exit(0)
