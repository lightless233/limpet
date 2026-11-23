// worker 经 systemd-run 启动的验收（spec 005 验收 9、11 与验收 10 的环境转发部分）。
// 只对 LIMPET_WORKER_LAUNCH=systemd-run 的 server 有效，其他方式打印「跳过」并以 0 退出。
//   1. 新建会话的 worker 在自己的临时单元里（控制组为 limpet-worker-<uuid>.service），父进程不是 server。
//   2. 环境由 server 转交：会话里的 LANG / PATH 与 server 的一致，LIMPET_* 配置项不进会话。
//   3. worker 日志仍落在 ~/.limpet/workers/<uuid>.log。
//   4. 关闭会话后单元消失（--collect）。
//   5. worker 被 kill -9 后，它的后台作业与 Shell 由 systemd 随单元清理，单元不残留。
const { BASE, WS, LIMPET_HOME } = await import('./env.mjs')
const { execFileSync, spawnSync } = await import('node:child_process')
const { existsSync, readFileSync } = await import('node:fs')
const { homedir } = await import('node:os')
const { join } = await import('node:path')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
let pass = true
const has = (n, ok, d) => {
  console.log(`  [${ok ? '通过' : '失败'}] ${n}${ok ? '' : ' —— ' + (d ?? '')}`)
  if (!ok) pass = false
}
const unit = (id) => `limpet-worker-${id}.service`
const units = () => {
  const r = spawnSync('systemctl', ['--user', 'list-units', '--all', '--no-legend', '--plain', 'limpet-worker-*'], { encoding: 'utf8' })
  return r.status === 0 ? r.stdout : ''
}
const show = (id, prop) =>
  execFileSync('systemctl', ['--user', 'show', unit(id), '-p', prop, '--value'], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim()

async function create(name) {
  const r = await fetch(`${BASE}/api/term`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name }),
  })
  return (await r.json()).id
}
// 在会话里执行一条命令，读回它的输出（原始字节去掉转义序列）。
function runInSession(id, cmd) {
  return new Promise((resolve) => {
    const ws = new WebSocket(`${WS}/api/ws/term/${id}?cols=200&rows=30`)
    ws.binaryType = 'arraybuffer'
    const dec = new TextDecoder()
    let out = ''
    let ready = false
    let started = false
    const finish = () => {
      ws.close()
      resolve(out.replace(/\x1b\[[0-9;?]*[ -\/]*[@-~]/g, '').replace(/\x1b\][^\x07\x1b]*(\x07|\x1b\\)/g, ''))
    }
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') {
        if (JSON.parse(e.data).type === 'ready') ready = true
        return
      }
      out += dec.decode(e.data)
      if (ready && !started) {
        started = true
        ws.send(Buffer.from(`${cmd}\r`))
        setTimeout(finish, 1500)
      }
    }
    ws.onclose = () => resolve('')
    setTimeout(() => { try { ws.close() } catch {} }, 8000)
  })
}

const list = await (await fetch(`${BASE}/api/term`)).json()
for (const s of list.filter((x) => x.name === '启动方式' || x.name === '启动方式-kill')) {
  await fetch(`${BASE}/api/term/${s.id}/close`, { method: 'POST' })
  await sleep(3500)
}

const id = await create('启动方式')
await sleep(300)
if (!units().includes(unit(id))) {
  console.log(`server 不是 systemd-run 启动方式（没有 ${unit(id)}），跳过`)
  await fetch(`${BASE}/api/term/${id}/close`, { method: 'POST' })
  console.log('=> 跳过')
  process.exit(0)
}

console.log('1. 独立单元')
has(`worker 单元已加载且 active（${unit(id)}）`, units().includes(unit(id)) && show(id, 'ActiveState') === 'active')
const cg = show(id, 'ControlGroup')
has('控制组是独立单元，不在 server 的单元里', cg.endsWith('/' + unit(id)), cg)
const workerPid = Number(show(id, 'MainPID'))
const ppid = Number(execFileSync('ps', ['-o', 'ppid=', '-p', String(workerPid)]).toString().trim())
const serverPids = spawnSync('pgrep', ['-f', '[l]impet[-].*-server$']).stdout.toString().trim().split('\n').filter(Boolean).map(Number)
has(`worker 的父进程不是 server（worker ${workerPid} ← ${ppid}；server ${serverPids.join(',') || '未识别'}）`, workerPid > 0 && !serverPids.includes(ppid))

console.log('2. 环境转交')
// 交互 Shell 的 rc 会改写 PATH（用户自己的目录排到最前），所以用 server 启动时带的
// LAUNCH_PROBE_MARKER 做严格验证：它在会话里出现，才算环境确实被转交了。
const envOut = await runInSession(id, 'echo "LANG=<$LANG>"; echo "PATH=<$PATH>"; echo "LAUNCH=<${LIMPET_WORKER_LAUNCH:-unset}>"; echo "MARKER=<${LAUNCH_PROBE_MARKER:-unset}>"')
has(`会话里的 LANG 与 server 一致（期望 LANG=<${process.env.LANG ?? ''}>）`, envOut.includes(`LANG=<${process.env.LANG ?? ''}>`), JSON.stringify(envOut.slice(-200)))
has('会话的 PATH 含系统目录', envOut.includes('/usr/local/bin'), JSON.stringify(envOut.slice(-200)))
if (process.env.LAUNCH_PROBE_MARKER) {
  has(`server 启动时带的变量被转交（LAUNCH_PROBE_MARKER=${process.env.LAUNCH_PROBE_MARKER}）`, envOut.includes(`MARKER=<${process.env.LAUNCH_PROBE_MARKER}>`), JSON.stringify(envOut.slice(-200)))
} else {
  console.log('  [跳过] 严格的环境转交验证（server 启动时未设置 LAUNCH_PROBE_MARKER）')
}
has('LIMPET_* 配置项不进会话', envOut.includes('LAUNCH=<unset>'), JSON.stringify(envOut.slice(-160)))

console.log('3. 日志')
const wlog = join(LIMPET_HOME, 'workers', `${id}.log`)
has('worker 日志落在 ~/.limpet/workers/<uuid>.log', existsSync(wlog) && readFileSync(wlog, 'utf8').includes('就绪'))

console.log('4. 关闭后单元消失')
await fetch(`${BASE}/api/term/${id}/close`, { method: 'POST' })
await sleep(4000)
has(`关闭会话后 ${unit(id)} 不再存在`, !units().includes(unit(id)))

console.log('5. worker 被 kill -9')
const id2 = await create('启动方式-kill')
await sleep(300)
await runInSession(id2, 'sleep 300 &!')
const pid2 = Number(show(id2, 'MainPID'))
process.kill(pid2, 'SIGKILL')
let clean = false
const t0 = Date.now()
while (Date.now() - t0 < 10000) {
  const sleepers = spawnSync('pgrep', ['-f', '[s]leep 300$']).status === 0
  if (!sleepers && !units().includes(unit(id2))) { clean = true; break }
  await sleep(500)
}
has(`worker 被 kill -9 后，后台作业与单元在 ${((Date.now() - t0) / 1000).toFixed(1)}s 内清理干净`, clean)
const left = (await (await fetch(`${BASE}/api/term`)).json()).find((s) => s.id === id2)
await fetch(`${BASE}/api/term/${id2}/close`, { method: 'POST' }).catch(() => {})
console.log(`  （列表里该会话：${left ? '仍在' : '已移除'}——stale socket 由 server 在下一次探测时清理）`)

console.log(pass ? '=> 通过' : '=> 失败')
process.exit(pass ? 0 : 1)