// 聚焦复现：resize 后 pane 尺寸是否跟随客户端。
const { BASE } = await import('./env.mjs')
import { execSync } from 'node:child_process'
const enc = new TextEncoder()
const dec = new TextDecoder()
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

try { execSync('tmux -L limpet kill-server 2>/dev/null') } catch {}

const res = await fetch(`${BASE}/api/term`, {
  method: 'POST', headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ name: 'size-probe' }),
})
const { id } = await res.json()
console.log('created:', id)

const ws = new WebSocket(`${BASE.replace('http', 'ws')}/api/ws/term/${id}?cols=80&rows=30`)
ws.binaryType = 'arraybuffer'
let out = ''
ws.onmessage = (e) => { if (typeof e.data !== 'string') out += (typeof e.data === 'string' ? '' : dec.decode(e.data)) }
await new Promise((r) => { ws.onopen = r })
ws.send(JSON.stringify({ type: 'resize', cols: 41, rows: 30 }))
await sleep(2000) // 等 zsh 起完
ws.send(enc.encode('stty size\r'))
await sleep(1500)
console.log('输出末尾:', JSON.stringify(out.slice(-300)))
ws.close()
process.exit(0)