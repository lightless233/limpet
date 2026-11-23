// 备用屏重连验收（spec 003 验收 3 + 影子主屏网格）：
// 主屏留标记 → 进入备用屏留标记 → 浏览器断开 → 重连 → 重建流应同时包含
// 影子主屏内容与备用屏内容，且顺序正确（主屏段在切换序列之前）；
// 随后退出备用屏，会话应继续可用。备用屏标记用 UTF-8 八进制转义构造，
// 避免输入行回显里出现字面标记干扰断言。
const { BASE, WS } = await import('./env.mjs')
import { execSync } from 'node:child_process'
const dec = new TextDecoder()
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const ALT = '\u5907\u7528\u5c4f\u6807\u8bb0' // 备用屏标记
const OCT = '\\345\\244\\207\\347\\224\\250\\345\\261\\217\\346\\240\\207\\350\\256\\260' // 同上，八进制形式

function attach(id, query) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(`${WS}/api/ws/term/${id}${query}`)
    ws.binaryType = 'arraybuffer'
    const st = { ready: false, chunks: [], text: () => st.chunks.map((c) => dec.decode(c)).join('') }
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') {
        try {
          if (JSON.parse(e.data).type === 'ready') st.ready = true
        } catch {}
        return
      }
      st.chunks.push(new Uint8Array(e.data))
    }
    ws.onopen = () => resolve({ ws, st })
    ws.onerror = () => reject(new Error('ws error'))
  })
}

const { id } = await (await fetch(`${BASE}/api/term`, { method: 'POST' })).json()
console.log('创建:', id)

const a = await attach(id, '?cols=100&rows=30')
await new Promise((r) => {
  const t = setInterval(() => a.st.ready && (clearInterval(t), r()), 50)
})
a.ws.send(Buffer.from("echo 影子主屏标记; printf '\\033[?1049h\\033[2J\\033[H" + OCT + "'; sleep 60\r"))
await sleep(1200)
a.ws.close() // 在备用屏里断开
await sleep(300)

const b = await attach(id, '?cols=100&rows=30')
await new Promise((r) => {
  const t = setInterval(() => b.st.ready && (clearInterval(t), r()), 50)
})
await sleep(1500)
const text = b.st.text()
let pass = true
const has = (name, ok) => {
  console.log(`  [${ok ? '通过' : '失败'}] ${name}`)
  if (!ok) pass = false
}
has('重建流包含影子主屏标记', text.includes('影子主屏标记'))
has('重建流包含备用屏切换 \\x1b[?1049h', text.includes('\x1b[?1049h'))
has('重建流包含备用屏标记', text.includes(ALT))
const iPri = text.indexOf('影子主屏标记')
const iSw = text.indexOf('\x1b[?1049h', iPri >= 0 ? iPri : 0)
const iAltAfter = text.indexOf(ALT, iSw >= 0 ? iSw : 0)
has('顺序正确：主屏段在切换之前、备用屏内容在切换之后', iPri >= 0 && iSw > iPri && iAltAfter > iSw)

// 退出备用屏，会话应继续可用
b.ws.send(Buffer.from("printf '\\033[?1049l'; echo 回到主屏AA\r"))
await sleep(1200)
const text2 = b.st.text()
has('退出备用屏后会话继续可用（回到主屏AA）', text2.includes('回到主屏AA'))

b.ws.send(Buffer.from('exit\r'))
await sleep(600)

if (!pass) {
  console.log('=> 失败')
  process.exit(1)
}
console.log('=> 通过')
process.exit(0)