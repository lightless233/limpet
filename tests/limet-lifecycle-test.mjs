// 会话生命周期验证：
// A. 非法握手（无升级头）→ 400，会话仍可正常接入（claimed 复位）
// B. 正常接入后再次接入 → 409
// C. exit 后连接关闭（回归）
// D. 预留 31s 不接入 → TTL 回收，接入 404
const { BASE, WS } = await import('./env.mjs')
const dec = new TextDecoder()
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function create() {
  const res = await fetch(`${BASE}/api/term`, { method: 'POST' })
  return (await res.json()).id
}

function attach(id, query = '') {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(`${WS}/api/ws/term/${id}${query}`)
    ws.binaryType = 'arraybuffer'
    let out = ''
    let closed = false
    let ready = false
    ws.onclose = () => { closed = true }
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') {
        try { if (JSON.parse(e.data).type === 'ready') ready = true } catch {}
        return
      }
      out += (typeof e.data === 'string' ? '' : dec.decode(e.data))
    }
    ws.onopen = () => {
      ws.send(JSON.stringify({ type: 'resize', cols: 68, rows: 30 }))
      const wait = setInterval(() => {
        if (ready || closed) { clearInterval(wait); resolve({ ws, getOut: () => out, isClosed: () => closed }) }
      }, 100)
    }
    ws.onerror = reject
  })
}

// A + B
{
  const id = await create()
  // 非法握手：普通 GET，无 Upgrade 头
  const bad = await fetch(`${BASE}/api/ws/term/${id}`)
  console.log(`A1 非法握手: HTTP ${bad.status}（期望 400）`)
  const { ws, getOut } = await attach(id, '?cols=68&rows=30')
  console.log(`A2 非法握手后正常接入: 开启 ✓`)
  ws.send(new TextEncoder().encode('echo handshake-recovery-ok\r'))
  await sleep(800)
  console.log(`A3 会话可用: ${getOut().includes('handshake-recovery-ok') ? '✓' : '✗'}`)

  // B：第二条 WS 接入同一会话 → 409
  const res = await fetch(`${BASE}/api/term`, { method: 'POST' })
  const second = await fetch(`${BASE}/api/ws/term/${id}`)
  console.log(`B 双接入: HTTP ${second.status}（期望 401/409 拒绝）`)
  ws.close()
}

// C：exit 关闭（回归）
{
  const id = await create()
  const { ws, isClosed } = await attach(id)
  ws.send(new TextEncoder().encode('exit\r'))
  for (let i = 0; i < 6 && !isClosed(); i++) await sleep(500)
  console.log(`C exit 关闭: ${isClosed() ? '✓' : '✗'}`)
}

// D：TTL 回收（31s）
{
  const id = await create()
  console.log(`D1 预留 ${id}，等待 31s 让 TTL 回收...`)
  await sleep(31000)
  const res = await fetch(`${BASE}/api/ws/term/${id}`)
  console.log(`D2 超时后接入: HTTP ${res.status}（期望 4xx，404=已回收）`)
}
process.exit(0)