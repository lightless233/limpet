// 验收：ready 后粘贴 ~1.1MB 中文 → PTY 落盘 → 字节数 + md5 对账。
// 输入按 64KiB 分块（前端契约），含换行避免 canonical 模式 4096 行限。
import crypto from 'node:crypto'
const enc = new TextEncoder()
const dec = new TextDecoder()
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const BASE = 'http://127.0.0.1:4040'
const CHUNK = 64 * 1024

// ~1.1MB UTF-8 中文，每行 ~80 字符
const line = '中文粘贴测试——终端大段粘贴保真核对，混合标点、数字 1234567890 与英文 words。\n'
const payload = line.repeat(Math.ceil(1_100_000 / Buffer.byteLength(line, 'utf8')))
const payloadBytes = Buffer.from(payload, 'utf8')
const expectedMd5 = crypto.createHash('md5').update(payloadBytes).digest('hex')
console.log(`payload: ${payloadBytes.length} bytes, md5=${expectedMd5}`)

const res = await fetch(`${BASE}/api/term`, { method: 'POST' })
const { id } = await res.json()
const ws = new WebSocket(`${BASE.replace('http', 'ws')}/api/ws/term/${id}?cols=120&rows=40`)
ws.binaryType = 'arraybuffer'
let out = ''
let closed = false
ws.onclose = () => { closed = true }
ws.onmessage = (e) => {
  if (typeof e.data === 'string') return // ready 帧
  out += dec.decode(e.data)
}
await new Promise((r) => { ws.onopen = r })
ws.send(JSON.stringify({ type: 'resize', cols: 120, rows: 40 }))
for (let i = 0; i < 50 && !out.length; i++) await sleep(100) // 等 ready+首屏

// cat 落盘 → 粘贴 → EOF → 校验
ws.send(enc.encode('cat > /tmp/limet-paste.bin\r'))
await sleep(400)
for (let i = 0; i < payloadBytes.length; i += CHUNK) {
  ws.send(payloadBytes.slice(i, i + CHUNK))
  if (i % (CHUNK * 8) === 0) await sleep(30) // 轻微节流，贴近真实粘贴
}
await sleep(300)
ws.send(enc.encode('\x04')) // Ctrl+D EOF
await sleep(500)
ws.send(enc.encode('wc -c < /tmp/limet-paste.bin; md5sum /tmp/limet-paste.bin\r'))
for (let i = 0; i < 30 && !out.includes('limet-paste.bin'); i++) await sleep(200)
await sleep(400)

const m = out.match(/([0-9a-f]{32})\s+\/tmp\/limet-paste\.bin/)
const wcMatch = out.match(/(\d+)\s*\n?\s*\/?tmp\/limet-paste\.bin/s)
console.log(`closed=${closed}`)
console.log(`字节数: 期望 ${payloadBytes.length}`)
console.log(`md5:    期望 ${expectedMd5}`)
console.log(`md5:    实际 ${m ? m[1] : '未找到'}  → ${m && m[1] === expectedMd5 ? '✓ 一致' : '✗ 不一致'}`)
ws.close()
process.exit(m && m[1] === expectedMd5 ? 0 : 1)
