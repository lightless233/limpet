// Spec 004：接入即 ready + 重建，resize 是尺寸申报，尺寸随输入者变化。
import assert from 'node:assert/strict'
const { BASE, WS } = await import('./env.mjs')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const sockets = []

async function waitFor(check, message) {
  const deadline = Date.now() + 5000
  while (!check() && Date.now() < deadline) await sleep(25)
  assert.ok(check(), message)
}

async function attach(id, query = '') {
  const ws = new WebSocket(`${WS}/api/ws/term/${id}${query}`)
  sockets.push(ws)
  ws.binaryType = 'arraybuffer'
  const state = { ready: false, outputBeforeReady: false, text: '', closeCode: 0 }
  const decoder = new TextDecoder()
  ws.onmessage = ({ data }) => {
    if (typeof data === 'string') {
      if (JSON.parse(data).type === 'ready') state.ready = true
    } else {
      if (!state.ready) state.outputBeforeReady = true
      state.text += decoder.decode(data, { stream: true })
    }
  }
  ws.onclose = ({ code }) => { state.closeCode = code }
  await waitFor(() => state.ready && state.text.length > 0, '未申报尺寸也应收到 ready 与重建输出')
  assert.equal(state.outputBeforeReady, false, 'ready 必须先于第一帧输出')
  return {
    ws,
    state,
    send: (text) => ws.send(Buffer.from(text + '\r')),
  }
}

async function checkSize(viewer, cols, rows) {
  const start = viewer.state.text.length
  viewer.send('stty size')
  await waitFor(() => viewer.state.text.slice(start).includes(`${rows} ${cols}\r\n`),
    `PTY 应为 ${cols}×${rows}，实际输出 ${JSON.stringify(viewer.state.text.slice(start, start + 512))}`)
}

let id
try {
  const res = await fetch(`${BASE}/api/term`, { method: 'POST' })
  assert.equal(res.status, 200, '创建会话')
  const created = await res.json()
  id = created.id
  const a = await attach(id, '?cols=68&rows=30')
  const b = await attach(id)
  await checkSize(a, 80, 24)

  a.ws.send(JSON.stringify({ type: 'resize', cols: 41, rows: 30 }))
  // 等申报到达 worker，随后检查观察者的输入尺寸。
  await sleep(150)
  await checkSize(b, 80, 24)
  await checkSize(a, 41, 30)
  console.log('接入即重建，ready 先于输出，尺寸仅在申报者输入时生效')

  a.ws.close()
  await checkSize(b, 41, 30)
  b.send('exit')
  await waitFor(() => b.state.closeCode !== 0, 'Shell exit 应结束 viewer')
  assert.equal(b.state.closeCode, 1000)
  console.log('一个 viewer 断开后其余 viewer 可继续使用，Shell exit 收到 1000')
} finally {
  for (const ws of sockets) ws.close()
  if (id) await fetch(`${BASE}/api/term/${id}/close`, { method: 'POST' }).catch(() => {})
}
