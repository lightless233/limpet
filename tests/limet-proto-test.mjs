// 新启动协议综合验证（v2）：
//   sendInput(s)  → 二进制帧（终端输入）
//   sendControl(o)→ 文本帧（控制 JSON）
// A. ready 帧先于任何终端输出；B. 首条 resize 决定启动尺寸（URL 旧值不生效）；
// C. 超时兜底（query 尺寸）；D. 预启动输入缓冲；E. 大粘贴；F. exit 关闭。
const { BASE } = await import('./env.mjs')
const enc = new TextEncoder()
const dec = new TextDecoder()
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function create() {
  const res = await fetch(`${BASE}/api/term`, { method: 'POST' })
  return (await res.json()).id
}

function attach(id, query = '') {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(`${BASE.replace('http', 'ws')}/api/ws/term/${id}${query}`)
    ws.binaryType = 'arraybuffer'
    const state = { out: '', closed: false, closeCode: null, readyAt: null, firstBinaryAt: null }
    const t0 = performance.now()
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') {
        try { if (JSON.parse(e.data).type === 'ready') state.readyAt = Math.round(performance.now() - t0) } catch {}
        return
      }
      state.out += (typeof e.data === 'string' ? '' : dec.decode(e.data))
      if (state.firstBinaryAt === null) state.firstBinaryAt = Math.round(performance.now() - t0)
    }
    ws.onclose = (e) => { state.closed = true; state.closeCode = e.code }
    ws.onopen = () => resolve({
      ws, state,
      sendInput(s) { ws.send(typeof s === 'string' ? enc.encode(s) : s) },
      sendControl(o) { ws.send(JSON.stringify(o)) },
    })
    ws.onerror = () => {}
  })
}

function fillers(out) {
  const re = /\x1b\[7m%\x1b\[27m\x1b\[1m\x1b\[0m( +)\r/g
  const r = []
  let m
  while ((m = re.exec(out))) r.push(m[1].length + 1)
  return r
}

async function waitReady(s, timeoutMs = 5000) {
  const start = performance.now()
  while (s.readyAt === null && performance.now() - start < timeoutMs) await sleep(100)
}

// A + B
{
  const id = await create()
  const { state, sendInput, sendControl } = await attach(id, '?cols=80&rows=30')
  sendControl({ type: 'resize', cols: 41, rows: 30 })
  await waitReady(state)
  sendInput('echo limpet-proto-ok\r')
  await sleep(1200)
  console.log(`A ready帧: ${state.readyAt}ms，先于输出: ${state.firstBinaryAt === null || state.readyAt <= state.firstBinaryAt ? '✓' : '✗'}`)
  console.log(`B 启动尺寸: [${fillers(state.out).join(', ')}]（期望 [41]，URL 的 80 不生效）`)
  console.log(`B 会话可用: ${state.out.includes('limpet-proto-ok') ? '✓' : '✗'}`)
  sendInput('exit\r')
  await sleep(700)
}

// C：无 resize → 超时按 query 兜底启动；ready 后等足时间看首屏
{
  const id = await create()
  const { ws, state } = await attach(id, '?cols=68&rows=30')
  await waitReady(state, 5000)
  await sleep(1500)
  console.log(`C 超时兜底: ready@${state.readyAt}ms（期望≈3000），首屏尺寸 ${fillers(state.out)[0] ?? '?'}（期望 68）`)
  ws.close()
  await sleep(300)
}

// D1：接入后立即断开（不发 resize）→ 会话条目立即回收（再接入 404）
{
  const id = await create()
  const { ws } = await attach(id, '?cols=68&rows=30')
  await sleep(100)
  ws.close()
  await sleep(200)
  const res = await fetch(`${BASE}/api/ws/term/${id}`)
  console.log(`D1 未启动断开回收: HTTP ${res.status}（期望 404）`)
}

// D2：启动前发二进制输入 → 1008 terminal not ready + 条目回收
{
  const id = await create()
  const { ws, state, sendInput } = await attach(id, '?cols=68&rows=30')
  sendInput('echo premature\r')
  for (let i = 0; i < 20 && !state.closed; i++) await sleep(100)
  const res = await fetch(`${BASE}/api/ws/term/${id}`)
  console.log(`D2 预启动输入拒绝: closed=${state.closed} code=${state.closeCode}（期望 1008） 回收=${res.status}（期望 404）`)
}

// E：大粘贴（60KB 单条）
{
  const id = await create()
  const { state, sendInput, sendControl } = await attach(id, '?cols=68&rows=30')
  sendControl({ type: 'resize', cols: 68, rows: 30 })
  await waitReady(state)
  sendInput('echo ' + 'A'.repeat(60000) + '\r')
  await sleep(1200)
  sendInput('echo limpet-paste-alive\r')
  await sleep(900)
  console.log(`E 大粘贴: closed=${state.closed} 回显=${state.out.includes('AAAA') ? '✓' : '✗'} 存活=${state.out.includes('limpet-paste-alive') ? '✓' : '✗'}`)
  sendInput('exit\r')
  await sleep(600)
}
process.exit(0)