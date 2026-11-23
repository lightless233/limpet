// U5 验证：detach 前产生大量历史（200 行，屏幕只有 ~30 行），
// 重连后接收流应包含滚出屏幕的早期历史行（line-5 / line-20）。
const { BASE } = await import('./env.mjs')
const enc = new TextEncoder()
const dec = new TextDecoder()
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function create(name) {
  const res = await fetch(`${BASE}/api/term`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name }),
  })
  return (await res.json()).id
}

function attach(id, query = '') {
  return new Promise((resolve) => {
    const ws = new WebSocket(`${BASE.replace('http', 'ws')}/api/ws/term/${encodeURIComponent(id)}${query}`)
    ws.binaryType = 'arraybuffer'
    const state = { out: '', readyAt: null }
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') {
        try { if (JSON.parse(e.data).type === 'ready') state.readyAt = true } catch {}
        return
      }
      state.out += (typeof e.data === 'string' ? '' : dec.decode(e.data))
    }
    const h = {
      ws, state,
      sendInput(s) { ws.send(typeof s === 'string' ? enc.encode(s) : s) },
      sendControl(o) { ws.send(JSON.stringify(o)) },
    }
    ws.onclose = () => resolve(h)
    ws.onopen = () => resolve(h)
  })
}

async function waitReady(s, ms = 8000) {
  const t0 = performance.now()
  while (!s.readyAt && performance.now() - t0 < ms) await sleep(100)
}

const id = await create(`replay-probe-${Date.now() % 100000}`)
const first = await attach(id, '?cols=80&rows=30')
first.sendControl({ type: 'resize', cols: 80, rows: 30 })
await waitReady(first.state)
await sleep(2500) // zsh 冷启动
first.sendInput('for i in $(seq 1 200); do echo line-$i; done\r')
await sleep(2000)
const firstHasDeep = first.state.out.includes('line-5') && first.state.out.includes('line-199')
console.log(`首轮: 200 行输出=${firstHasDeep ? '✓' : '✗'}`)
first.ws.close()
await sleep(400)

const second = await attach(id, '?cols=80&rows=30')
second.sendControl({ type: 'resize', cols: 80, rows: 30 })
await waitReady(second.state)
await sleep(1500)
const replayDeep = second.state.out.includes('line-5') && second.state.out.includes('line-20')
console.log(`重连: ready=${second.state.readyAt ? '✓' : '✗'} 回放含早期历史(line-5/line-20)=${replayDeep ? '✓' : '✗'} 当前屏含 line-200=${second.state.out.includes('line-200') ? '✓' : '✗'}`)
second.sendInput('exit\r')
await sleep(500)
process.exit(replayDeep ? 0 : 1)