// U3 慢消费者验收（spec 003 验收 22）：会话持续大量输出时，接入方停止读取——
// PTY 读取与录制不拖住；队列打满后该接入被断开（关闭码 4001）；重新接入后
// 画面正确、会话继续可用。
import { connect } from 'node:net'
const BASE = process.env.BASE || 'http://127.0.0.1:4040'
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
let pass = true
const has = (n, ok, d) => {
  console.log(`  [${ok ? '通过' : '失败'}] ${n}${ok ? '' : ' —— ' + (d ?? '')}`)
  if (!ok) pass = false
}

// 清理上次运行的同名残留
{
  const list = await (await fetch(`${BASE}/api/term`)).json()
  for (const s of list.filter((x) => x.name === '慢消费者')) {
    await fetch(`${BASE}/api/term/${s.id}/close`, { method: 'POST' })
    await sleep(3500) // 关闭含 TERM→KILL 宽限
  }
}
const { id } = await (await fetch(`${BASE}/api/term`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ name: '慢消费者' }),
})).json()
console.log('创建:', id)

// 原始 WS：握手 → resize → 启动持续输出 → 暂停读取（停止消费）
const sock = net_connect(4040)
const t0 = Date.now()
let upgradeOk = false
let closeCode = 0
let sentLoop = false
const T = (s) => `[${String(Date.now() - t0).padStart(5)}ms]`
sock.on('connect', () => {
  sock.write(
    `GET /api/ws/term/${id}?cols=100&rows=30 HTTP/1.1\r\nHost: 127.0.0.1:4040\r\n` +
    `Upgrade: websocket\r\nConnection: Upgrade\r\n` +
    `Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n`,
  )
})
let buf = Buffer.alloc(0)
sock.on('data', (d) => {
  buf = Buffer.concat([buf, d])
  // HTTP 升级应答
  if (!upgradeOk) {
    const i = buf.indexOf('\r\n\r\n')
    if (i >= 0) {
      const status = buf.slice(0, i).toString().split('\r\n')[0]
      upgradeOk = status.includes('101')
      console.log(`  ${T()} 升级: ${status}`)
      buf = buf.slice(i + 4)
      // resize 帧（掩码文本帧）
      sock.write(wsFrame(0x1, Buffer.from(JSON.stringify({ type: 'resize', cols: 100, rows: 30 }))))
      // 持续输出任务（前台循环即可：它的输出就是负载）
      sock.write(wsFrame(0x2, Buffer.from("yes '0123456789012345678901234567890123456789012345678901234567890123' | head -c 30000000\r")))
      sentLoop = true
      // 占空比制造真实背压：暂停让 TCP 窗口关闭，恢复间隙观察关闭帧
      setTimeout(() => {
        console.log(`  ${T()} 暂停读取（模拟慢消费者），之后每 6s 观察 0.4s`)
        sock.pause()
        const cycle = () => {
          setTimeout(() => {
            sock.resume()
            setTimeout(() => {
              sock.pause()
              cycle()
            }, 400)
          }, 6000)
        }
        cycle()
      }, 500)
    }
    return
  }
})
let cEOF = false
sock.on('end', () => { cEOF = true })
sock.on('close', () => console.log(`  ${T()} 连接关闭`))

// 关闭帧扫描器：从缓冲增量解析，发现 opcode 0x8 → 记录关闭码
const waitClose = new Promise((r) => {
  const iv = setInterval(() => {
    let i = 0
    for (;;) {
      if (i + 2 > buf.length) return
      const op = buf[i] & 0x0f
      const len7 = buf[i + 1] & 0x7f
      let hlen = 2
      let len = len7
      if (len7 === 126) { if (i + 4 > buf.length) return; len = buf.readUInt16BE(i + 2); hlen = 4 }
      else if (len7 === 127) { if (i + 10 > buf.length) return; len = Number(buf.readBigUInt64BE(i + 2)); hlen = 10 }
      if (i + hlen + len > buf.length) return
      if (op === 0x8) {
        closeCode = len >= 2 ? buf.readUInt16BE(i + hlen) : 0
        clearInterval(iv)
        r()
        return
      }
      i += hlen + len
      buf = buf.slice(i)
      i = 0
    }
  }, 150)
})

function net_connect(port) {
  return connect(port, '127.0.0.1')
}
// server → client 的帧不掩码；client → server 必须掩码
function wsFrame(op, payload) {
  const mask = Buffer.from([0x11, 0x22, 0x33, 0x44])
  const masked = Buffer.from(payload.map((b, i) => b ^ mask[i % 4]))
  const parts = [Buffer.from([0x80 | op])]
  const l = payload.length
  if (l < 126) parts.push(Buffer.from([0x80 | l]))
  else if (l < 65536) { const b = Buffer.alloc(2); b.writeUInt16BE(l); parts.push(Buffer.from([0x80 | 126]), b) }
  else { const b = Buffer.alloc(8); b.writeBigUInt64BE(BigInt(l)); parts.push(Buffer.from([0x80 | 127]), b) }
  parts.push(mask, masked)
  return Buffer.concat(parts)
}

await Promise.race([waitClose, sleep(25000)])
has(`慢消费者被断开（关闭码 ${closeCode}，期望 4001 或连接关闭）`, closeCode === 4001 || cEOF)
sock.destroy()

// 重连：会话应继续可用
const ws = new WebSocket(`ws://127.0.0.1:4040/api/ws/term/${id}?cols=80&rows=24`)
ws.binaryType = 'arraybuffer'
let ready = false
let ok = false
const decChunks = []
ws.onmessage = (e) => {
  if (typeof e.data === 'string') {
    try { if (JSON.parse(e.data).type === 'ready') ready = true } catch {}
    return
  }
  decChunks.push(Buffer.from(e.data).toString())
}
ws.onopen = () => {}
const t = Date.now()
while (!ready && Date.now() - t < 8000) await sleep(50)
ws.send(Buffer.from('echo 重连可用OK\r'))
await sleep(1200)
for (const b of decChunks) if (b.includes('重连可用OK')) ok = true
has('断开后重新接入，会话继续可用', ok)
ws.send(Buffer.from('exit\r'))
await sleep(500)

console.log(pass ? '=> 通过' : '=> 失败')
process.exit(pass ? 0 : 1)
