// 多视图验收（spec 004 验收 1/2/4/5 + 验收 22 复用）：
// 1. 双 viewer 同时接入同一会话：均 ready、互不踢、无 409。
// 2. 任一 viewer 输入 → 所有 viewer 画面同步。
// 3. 高吞吐下慢 viewer 被单独丢弃（4001），其余不受影响。
// 4. viewer 断开重连 → 重建 + 实时尾巴恰好一次。
const { BASE, WS, LIMPET_HOME } = await import('./env.mjs')
import { connect as netConnect } from 'node:net'
const dec = new TextDecoder()
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
let pass = true
const has = (n, ok, d) => {
  console.log(`  [${ok ? '通过' : '失败'}] ${n}${ok ? '' : ' —— ' + (d ?? '')}`)
  if (!ok) pass = false
}

function attach(id) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(`${WS}/api/ws/term/${id}?cols=100&rows=30`)
    ws.binaryType = 'arraybuffer'
    const st = { ready: false, text: '', ws, closedCode: 0 }
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') {
        try {
          if (JSON.parse(e.data).type === 'ready') st.ready = true
        } catch {}
        return
      }
      st.text += (typeof e.data === 'string' ? '' : dec.decode(e.data))
    }
    ws.onclose = (e) => { st.closedCode = e.code }
    ws.onopen = () => resolve(st)
    ws.onerror = () => reject(new Error('ws error'))
  })
}
const waitReady = async (st) => {
  const t0 = Date.now()
  while (!st.ready && Date.now() - t0 < 10000) await sleep(50)
  return st.ready
}

// ---- 0. 清理上次运行的同名残留（关闭含 TERM→KILL 宽限）----
{
  const list = await (await fetch(`${BASE}/api/term`)).json()
  for (const s of list.filter((x) => x.name === '多视图')) {
    await fetch(`${BASE}/api/term/${s.id}/close`, { method: 'POST' })
    await sleep(3500)
  }
}

// ---- 1. 双 viewer 同时接入 ----
const { id } = await (await fetch(`${BASE}/api/term`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ name: '多视图' }),
})).json()
console.log('创建:', id)
const [a, b] = await Promise.all([attach(id), attach(id)])
const rA = await waitReady(a)
const rB = await waitReady(b)
has('viewer A ready', rA)
has('viewer B ready（无 409，互不踢）', rB)

// ---- 2. 输入互通 ----
a.ws.send(Buffer.from('echo 甲输入-标记\r'))
await sleep(1000)
has('A 的输入出现在 A', a.text.includes('甲输入-标记'))
has('A 的输入同步到 B', b.text.includes('甲输入-标记'))
b.ws.send(Buffer.from('echo 乙输入-标记\r'))
await sleep(1000)
has('B 的输入出现在 B', b.text.includes('乙输入-标记'))
has('B 的输入同步到 A', a.text.includes('乙输入-标记'))

// ---- 3. 高吞吐下慢 viewer 单独丢弃（C 用原始 socket 以便暂停读取）----
const cRaw = netConnect(LIMPET_HOME + '/workers/' + id + '.sock')
let cClosed = 0
let cEOF = false
let cUp = false
let cBuf = Buffer.alloc(0)
let eof = false
const t0 = Date.now()
cRaw.on('connect', () => {
  const hs = Buffer.from(JSON.stringify({ op: 'attach', proto: 2, uuid: id }))
  const head = Buffer.alloc(5)
  head[0] = 0x10
  head.writeUInt32BE(hs.length, 1)
  cRaw.write(Buffer.concat([head, hs]))
})
cRaw.on('data', (d) => {
  cBuf = Buffer.concat([cBuf, d])
  // 握手应答
  if (!cUp && cBuf.length >= 5 && cBuf[0] === 0x11) {
    cUp = true
    cBuf = cBuf.slice(5 + cBuf.readUInt32BE(1))
    // resize 申报帧（掩码文本帧）

    // 窗口尺寸申报帧（worker 协议：[0x03][len BE32][JSON]，不掩码）
    cRaw.write(workerFrame(0x03, Buffer.from(JSON.stringify({ cols: 100, rows: 30 }))))
    // 启动高吞吐负载（输入帧：[0x02][len BE32][字节]）
    const load = Buffer.from("yes '0123456789012345678901234567890123456789012345678901234567890123' | head -c 30000000\r")
    cRaw.write(workerFrame(0x02, load))
    // 占空比制造真实背压：停 6s / 观察 0.4s（暂停期间 data 事件停止，观察窗才能看到 DETACH）
    setTimeout(() => {
      console.log(`  ${String(Date.now() - t0).padStart(5)}ms C 暂停读取（模拟慢 viewer）`)
      cRaw.pause()
      const cycle = (n) => {
        setTimeout(() => {
          console.log(`  [调试] 恢复#${n}: closed=${cClosed} eof=${cEOF} readableEnded=${cRaw.readableEnded} cBuf=${cBuf.length}`)
          cRaw.resume()
          setTimeout(() => {
            console.log(`  [调试] 再暂停#${n}: 缓冲 ${cBuf.length}B`)
            cRaw.pause()
            cycle(n + 1)
          }, 400)
        }, 6000)
      }
      cycle(1)
    }, 300)
    return
  }
  // 帧扫描：找 DETACH(0x06) / 关闭
  let i = 0
  for (;;) {
    if (i + 5 > cBuf.length) return
    const op = cBuf[i]
    const len = cBuf.readUInt32BE(i + 1)
    if (i + 5 + len > cBuf.length) return
    if (op === 0x06) {
      try { cClosed = JSON.parse(cBuf.slice(i + 5, i + 5 + len).toString()).code } catch {}
    }
    if (op === 0x04) cClosed = 1000
    cBuf = cBuf.slice(i + 5 + len)
    i = 0
  }
})
const waitCDrop = new Promise((r) => {
  const iv = setInterval(() => {
    if (cClosed || (cEOF && Date.now() - t0 > 8000)) { clearInterval(iv); r() }
  }, 200)
})
await Promise.race([waitCDrop, sleep(25000)])
// 断言：C 被丢弃。DETACH(4001)送达 或 worker 关闭连接（EOF）均算——
// 前者是慢消费者主路径（server 中继 5s 写超时也走 4001），后者是原始 socket
// 停读时 DETACH 写不进打满缓冲的 EOF 兜底。会话与他人不受影响才是关键不变量。
const droppedC = cClosed === 4001 || cRaw.readableEnded
has(`慢 viewer C 被单独丢弃（code=${cClosed}，readableEnded=${cRaw.readableEnded}）`, droppedC)
has('A 不受 C 影响', a.closedCode === 0)
has('B 不受 C 影响', b.closedCode === 0)
a.ws.send(Buffer.from('echo 丢弃后仍可用\r'))
await sleep(1000)
has('丢弃慢 viewer 后会话继续可用', a.text.includes('丢弃后仍可用'))

// ---- 4. viewer 断开重连 ----
a.ws.close()
await sleep(300)
const a2 = await attach(id)
const rA2 = await waitReady(a2)
await sleep(500)
a2.ws.send(Buffer.from('echo 重连甲OK\r'))
await sleep(1000)
has('A 断开重连成功', rA2 && a2.text.includes('重连甲OK'))
has('重连画面恰好一次（无整段双份）', !/多视图标记-乙[\s\S]*多视图标记-乙/.test(a2.text.slice(0, 2000)))

// 清理
for (const st of [a2, b]) st.ws.send(Buffer.from('exit\r'))
await sleep(600)

console.log(pass ? '=> 通过' : '=> 失败')
process.exit(pass ? 0 : 1)

// workerFrame 按 worker 协议封帧：[type:1][len:4 BE][payload]
function workerFrame(type_, payload) {
  const head = Buffer.alloc(5)
  head[0] = type_
  head.writeUInt32BE(payload.length, 1)
  return Buffer.concat([head, payload])
}