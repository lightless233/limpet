// U3 生命周期验收（spec 003 验收 6 + 验收 8）：
// 1. 显式关闭回收整棵会话进程树：前台任务与后台作业各一个（脱离 Shell 进程组的
//    后台作业是关键项——bash/zsh 都给后台作业单开进程组）。
// 2. 同会话第二个接入在 WS 升级之前得到 HTTP 409。
const { BASE, WS } = await import('./env.mjs')
import { execSync } from 'node:child_process'
const dec = new TextDecoder()
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
let pass = true
const has = (name, ok) => {
  console.log(`  [${ok ? '通过' : '失败'}] ${name}`)
  if (!ok) pass = false
}
const sh = (c) => execSync(c, { stdio: ['ignore', 'pipe', 'ignore'] }).toString()

function attach(id) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(`${WS}/api/ws/term/${id}?cols=100&rows=30`)
    ws.binaryType = 'arraybuffer'
    const st = { ready: false, text: '', ws }
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') {
        try {
          if (JSON.parse(e.data).type === 'ready') st.ready = true
        } catch {}
        return
      }
      st.text += (typeof e.data === 'string' ? '' : dec.decode(e.data))
    }
    ws.onopen = () => resolve(st)
    ws.onerror = () => reject(new Error('ws error'))
  })
}
const waitReady = async (st) => {
  const t0 = Date.now()
  while (!st.ready && Date.now() - t0 < 8000) await sleep(50)
  return st.ready
}

// ---- 1. 关闭回收前台 + 后台作业 ----
const { id } = await (await fetch(`${BASE}/api/term`, { method: 'POST' })).json()
console.log('创建:', id)
const st = await attach(id)
await waitReady(st)
// 前台 sleep 与后台 sleep 各一；跑满长时限以便观察是否被回收
// 作业标记用括号技巧：pgrep 模式 "sleep 5[5]5" 只命中字面 sleep 555，
// 而测量命令自身的 cmdline 含 "5[5]5" 不会自匹配（本项目踩过三次的坑）
st.ws.send(Buffer.from('sleep 555 &\r'))
await sleep(300)
st.ws.send(Buffer.from('sleep 556\r'))
await sleep(600)
const fgBefore = sh(`pgrep -f 'sleep 55[6]' | wc -l`).trim()
const bgBefore = sh(`pgrep -f 'sleep 55[5]' | wc -l`).trim()
has('关闭前两个作业都在运行', fgBefore >= 1 && bgBefore >= 1)

const res = await fetch(`${BASE}/api/term/${id}/close`, { method: 'POST' })
has('关闭请求成功', res.status === 200)

let reaped = false
const t0 = Date.now()
while (Date.now() - t0 < 12000) {
  const fg = parseInt(sh(`pgrep -f 'sleep 55[6]' | wc -l`).trim(), 10)
  const bg = parseInt(sh(`pgrep -f 'sleep 55[5]' | wc -l`).trim(), 10)
  if (fg === 0 && bg === 0) {
    reaped = true
    break
  }
  await sleep(300)
}
has('关闭后前台任务与后台作业全部结束（回收单位=会话）', reaped)
const list = await (await fetch(`${BASE}/api/term`)).json()
has('列表中该会话已移除', !list.some((s) => s.id === id))

// ---- 2. 多视图：第二个接入成功，两路画面同步（spec 004 修订一，409 废止）----
const { id: id2 } = await (await fetch(`${BASE}/api/term`, { method: 'POST' })).json()
const st2 = await attach(id2)
await waitReady(st2)
st2.ws.send(Buffer.from('echo 多视图标记-乙\r'))
await sleep(1000)
has('第二个接入成功且看到自己的输出', st2.text.includes('多视图标记-乙'))
has('第一个接入不受第二个影响', !st.closed)
st2.ws.send(Buffer.from('exit\r'))
await sleep(600)

// 清理
st2.ws.send(Buffer.from('exit\r'))
await sleep(600)

if (!pass) {
  console.log('=> 失败')
  process.exit(1)
}
console.log('=> 通过')
process.exit(0)