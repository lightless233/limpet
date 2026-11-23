// 心跳验收（spec 005 验收 1、2），运行约 70 秒：
// 1. server 每 20s 发一帧 {"type":"ping"}；应答 pong 的连接跨多个周期保持不断。
// 2. 只读不应答的客户端约 60s 被以 4002 关闭；应答 pong 的 viewer 与「只输入不回 pong」
//    的 viewer 都不受影响（输入同样算听到对端）。
// 3. 全部 viewer 退出后，列表里该会话的「已接入」解除——4002 的连接确实从 worker 摘除。
// 4. ping/pong 不进 worker：终端输出里不出现 pong 字样。
const { BASE, WS } = await import('./env.mjs')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
let pass = true
const has = (n, ok, d) => {
  console.log(`  [${ok ? '通过' : '失败'}] ${n}${ok ? '' : ' —— ' + (d ?? '')}`)
  if (!ok) pass = false
}

// attach 建一个 viewer。opts.pong：收到 ping 回 pong；opts.typeEvery：每隔 N 毫秒发一条输入。
function attach(id, opts = {}) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(`${WS}/api/ws/term/${id}?cols=100&rows=30`)
    ws.binaryType = 'arraybuffer'
    const dec = new TextDecoder()
    const st = { ws, at: Date.now(), ready: false, pings: [], text: '', closedCode: 0, closedAt: 0, typed: 0 }
    let timer = 0
    ws.onopen = () => {
      if (opts.typeEvery) {
        timer = setInterval(() => {
          st.typed++
          if (ws.readyState === WebSocket.OPEN) ws.send(Buffer.from(`echo c-${st.typed}\r`))
        }, opts.typeEvery)
      }
      resolve(st)
    }
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') {
        try {
          const ctl = JSON.parse(e.data)
          if (ctl.type === 'ready') st.ready = true
          if (ctl.type === 'ping') {
            st.pings.push(Date.now())
            if (opts.pong && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'pong' }))
          }
        } catch {}
        return
      }
      st.text += dec.decode(e.data)
    }
    ws.onclose = (e) => {
      st.closedCode = e.code
      st.closedAt = Date.now()
      clearInterval(timer)
    }
    ws.onerror = () => reject(new Error('ws error'))
  })
}
const waitReady = async (st) => {
  const t0 = Date.now()
  while (!st.ready && Date.now() - t0 < 10000) await sleep(50)
  return st.ready
}

// ---- 0. 清理上次运行的同名残留 ----
{
  const list = await (await fetch(`${BASE}/api/term`)).json()
  for (const s of list.filter((x) => x.name === '心跳')) {
    await fetch(`${BASE}/api/term/${s.id}/close`, { method: 'POST' })
    await sleep(3500)
  }
}

// ---- 1. 三个 viewer 同时接入同一会话 ----
const { id } = await (await fetch(`${BASE}/api/term`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ name: '心跳' }),
})).json()
console.log('创建:', id)
const a = await attach(id, { pong: true }) // 应答 pong
const b = await attach(id) // 只读不应答
const c = await attach(id, { typeEvery: 10000 }) // 只输入，不回 pong
has('A ready', await waitReady(a))
has('B ready', await waitReady(b))
has('C ready', await waitReady(c))

// ---- 2. 等待 B 被判定失联（约 60s）----
const t0 = Date.now()
while (!b.closedCode && Date.now() - t0 < 80000) await sleep(200)
const bLife = (b.closedAt - b.at) / 1000
has(`只读不应答的 B 被以 4002 关闭（存活 ${bLife.toFixed(1)}s）`, b.closedCode === 4002, `code=${b.closedCode}`)
has('关闭时刻在 55–75s 之间（约 60s）', bLife >= 55 && bLife <= 75)

// ---- 3. 其余 viewer 不受影响 ----
await sleep(1500) // 给 4002 之后的一个观察窗
has('应答 pong 的 A 仍然连接', a.closedCode === 0 && a.ws.readyState === WebSocket.OPEN)
has('只输入不回 pong 的 C 仍然连接（输入同样算听到对端）', c.closedCode === 0 && c.ws.readyState === WebSocket.OPEN)
has('C 的输入持续得到回显', (c.text.match(/c-\d+/g) || []).length >= 5, `回显 ${(c.text.match(/c-\d+/g) || []).length} 条`)
const gaps = a.pings.slice(1).map((t, i) => (t - a.pings[i]) / 1000)
has(`A 收到 ${a.pings.length} 帧 ping（≥ 2）`, a.pings.length >= 2)
has(`ping 间隔 20s ± 1s（实测 ${gaps.map((g) => g.toFixed(1)).join(', ')}s）`, gaps.every((g) => g >= 19 && g <= 21))
has('ping/pong 不进 worker（终端输出不含 pong 字样）', !a.text.includes('pong'))

// ---- 4. 全部 viewer 退出后「已接入」解除 ----
for (const st of [a, c]) st.ws.close()
let attached = true
for (let i = 0; i < 30 && attached; i++) {
  await sleep(200)
  attached = ((await (await fetch(`${BASE}/api/term`)).json()).find((s) => s.id === id) ?? { attached: true }).attached
}
has('全部 viewer 退出后该会话「已接入」解除', !attached)

// 清理
await fetch(`${BASE}/api/term/${id}/close`, { method: 'POST' })
console.log(pass ? '=> 通过' : '=> 失败')
process.exit(pass ? 0 : 1)