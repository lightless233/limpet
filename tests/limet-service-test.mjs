// 服务方式验收（spec 005 验收 8、9）：server 作为 systemd 用户单元运行时，stop / restart / 被 kill -9
// 之后会话都存活并能在服务回来后接回；worker 始终在自己的单元里，Shell 的 PID 不变。
//
//   UNIT=limpet-verify BASE=http://127.0.0.1:13001 LIMPET_HOME=/tmp/limpet-verify-home node tests/limet-service-test.mjs
//
// UNIT 是被测服务的单元名（不带 .service）；BASE 与 LIMPET_HOME 必须与该服务的一致。
// 脚本会停、重启这个服务——不要对着正在使用的正式服务跑。
const { BASE, WS, LIMPET_HOME } = await import('./env.mjs')
const { execFileSync, spawnSync } = await import('node:child_process')
const { readFileSync, existsSync } = await import('node:fs')
const { join } = await import('node:path')
const unit = (process.env.UNIT || 'limpet-server').replace(/\.service$/, '')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
let pass = true
const has = (n, ok, d) => {
  console.log(`  [${ok ? '通过' : '失败'}] ${n}${ok ? '' : ' —— ' + (d ?? '')}`)
  if (!ok) pass = false
}
const systemctl = (...a) => execFileSync('systemctl', ['--user', ...a], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim()
const show = (u, prop) => systemctl('show', u, '-p', prop, '--value')
// is-active 在单元不活跃时按约定以非零退出码结束，状态要看 stdout。
const active = (u) => {
  const r = spawnSync('systemctl', ['--user', 'is-active', u], { encoding: 'utf8' })
  return r.stdout.trim() || `exit ${r.status}`
}
// 单元 active（Type=exec：进程已 exec）不等于已在监听——以 API 可达为准。
const waitUp = async () => {
  const t0 = Date.now()
  while (Date.now() - t0 < 15000) {
    try {
      if ((await fetch(`${BASE}/api/term`)).ok) return true
    } catch {}
    await sleep(100)
  }
  return false
}
const attach = () =>
  new Promise((resolve) => {
    const ws = new WebSocket(`${WS}/api/ws/term/${id}`)
    ws.binaryType = 'arraybuffer'
    const dec = new TextDecoder()
    let out = ''
    let ready = false
    let sent = false
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') {
        if (JSON.parse(e.data).type === 'ready') ready = true
        return
      }
      out += dec.decode(e.data)
      if (ready && !sent) {
        sent = true
        ws.send(Buffer.from(`echo ${marker}\r`))
        setTimeout(() => {
          ws.close()
          resolve(out)
        }, 1200)
      }
    }
    ws.onclose = () => resolve(out)
  })

// server 单元控制组里的 PID（不应出现 worker）
const serverCgroupPids = () => {
  const cg = show(`${unit}.service`, 'ControlGroup')
  const f = `/sys/fs/cgroup${cg}/cgroup.procs`
  return existsSync(f) ? readFileSync(f, 'utf8').trim().split('\n').filter(Boolean) : []
}
const shellPid = (workerPid) => Number(execFileSync('ps', ['-o', 'pid=', '--ppid', String(workerPid)]).toString().trim().split('\n')[0])
// 在会话里执行一条命令并确认它到达 Shell（录制每批即落盘）。
const recorded = async (m) => {
  const log = join(LIMPET_HOME, 'history', `${id}.log`)
  const t0 = Date.now()
  while (Date.now() - t0 < 6000) {
    if (existsSync(log) && readFileSync(log, 'latin1').includes(m)) return true
    await sleep(150)
  }
  return false
}
let id = 0
let marker = ''
const listHas = async () => ((await (await fetch(`${BASE}/api/term`)).json()).some((s) => s.id === id))

console.log(`被测服务：${unit}.service @ ${BASE}（数据目录 ${LIMPET_HOME}）`)
id = (await (await fetch(`${BASE}/api/term`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: '服务验收' }) })).json()).id
await sleep(400)
const wu = `limpet-worker-${id}.service`
marker = 'BEFORE-1'
await attach()
has('接入并输入（BEFORE-1 到达 Shell）', await recorded('BEFORE-1'))
const shell0 = shellPid(Number(show(wu, 'MainPID')))

console.log('A. systemctl restart')
const restarts0 = Number(show(`${unit}.service`, 'NRestarts'))
systemctl('restart', `${unit}.service`)
has('服务回来（API 可达）', await waitUp())
await sleep(500)
has('worker 单元仍是 active', active(wu) === 'active')
has(`会话 Shell 的 PID 不变（${shell0}）`, shellPid(Number(show(wu, 'MainPID'))) === shell0)
has(`server 单元的控制组里只有 server 自己（${serverCgroupPids().join(',')}）`, serverCgroupPids().length === 1 && serverCgroupPids()[0] === show(`${unit}.service`, 'MainPID'))
has('列表里有该会话（名字保留）', await listHas())
marker = 'AFTER-RESTART'
await attach()
has('接回后可以继续输入（AFTER-RESTART 到达 Shell）', await recorded('AFTER-RESTART'))

console.log('B. systemctl stop → start')
systemctl('stop', `${unit}.service`)
has('服务已停止', active(`${unit}.service`) === 'inactive')
await sleep(500)
has('服务停止期间 worker 单元仍是 active', active(wu) === 'active')
has('服务停止期间会话 Shell 的 PID 不变', shellPid(Number(show(wu, 'MainPID'))) === shell0)
marker = 'WHILE-STOPPED'
// 服务不在，无法经它输入；这里只确认会话进程还在，恢复后再接。
systemctl('start', `${unit}.service`)
has('服务回来（API 可达）', await waitUp())
has('服务回来后列表里有该会话', await listHas())
marker = 'AFTER-START'
await attach()
has('接回后可以继续输入（AFTER-START 到达 Shell）', await recorded('AFTER-START'))

console.log('C. server 进程被 kill -9')
const pid = Number(show(`${unit}.service`, 'MainPID'))
const t0 = Date.now()
process.kill(pid, 'SIGKILL')
const up = await waitUp()
has(`≤ 5 秒内自动重启并恢复服务（${((Date.now() - t0) / 1000).toFixed(1)}s）`, up && Date.now() - t0 <= 5000)
has('NRestarts 递增', Number(show(`${unit}.service`, 'NRestarts')) === restarts0 + 1)
has('会话 Shell 的 PID 不变', shellPid(Number(show(wu, 'MainPID'))) === shell0)
has('列表里有该会话', await listHas())
marker = 'AFTER-KILL9'
await attach()
has('接回后可以继续输入（AFTER-KILL9 到达 Shell）', await recorded('AFTER-KILL9'))

await fetch(`${BASE}/api/term/${id}/close`, { method: 'POST' })
console.log(pass ? '=> 通过' : '=> 失败')
process.exit(pass ? 0 : 1)
