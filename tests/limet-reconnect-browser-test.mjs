// 前端自动重连的浏览器验收（spec 005 验收 4、5、6 与终态分流），运行约 2 分钟。
//
// 前置：本机有可用的 Playwright + Chromium（依次尝试 import 'playwright'、环境变量
// PLAYWRIGHT_MODULE、~/.npm/_npx 里与已缓存 Chromium 版本匹配的一份）；仓库里已有 web/dist。
// 脚本以 SERVER_BIN 指向的二进制启动临时 server（cwd 为仓库根），
// 浏览器经一个可干预的 TCP 中继（PORT+2）访问它，用中继模拟网络闪断、无声失联、登录失效。
// 要求 PORT（默认 4040）与 PORT+2 空闲。
//
//   S1 接入与输入：徽标「已连接」，按键到达 Shell
//   S2 server 重启：停 → 「重连中」→ 起 → 自动接回；重连不重放输入
//   S3 网络闪断：现有连接被切断 → 自动接回
//   S4 登录失效：核对请求被重定向 → 「登录已失效」+「刷新页面」
//   S5 无声失联：server → 浏览器方向停滞（没有任何帧到达）→ 60 秒内自行判定并接回
//   S6 Shell 内 exit → 「会话已结束」，不再重连
//   S7 列表里关闭会话 → 「会话已结束」
//   S8 worker 被 kill -9 → 核对后「会话已结束」
//   S9 移动视口（375）：接入、输入及快捷键状态
//   S10 列表：表单提示与单会话关闭中状态
// 设置环境变量 SHOTS=<目录> 时保存截图（接入后的桌面与移动视口各一张）。
import { execFileSync, spawn } from 'node:child_process'
import { existsSync, readFileSync, readdirSync } from 'node:fs'
import { createServer, connect } from 'node:net'
import { basename, dirname, join, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const PORT = Number(process.env.PORT || 4040)
const BASE = `http://127.0.0.1:${PORT}`
const ORIGIN = `http://127.0.0.1:${PORT + 2}` // 经中继访问
const SERVER_BIN = process.env.SERVER_BIN
if (!SERVER_BIN) throw new Error('请用 SERVER_BIN 指定本轮构建的测试二进制')
const runToken = `${Date.now().toString(36)}-${process.pid}`
const testName = (label) => `${label}-${runToken}`
const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const { LIMPET_HOME } = await import('./env.mjs') // 数据目录，随环境传给脚本起的 server
const { homedir } = await import('node:os')
const SHOTS = process.env.SHOTS // 截图目录（可选）
const shot = (pg, name) => (SHOTS ? pg.screenshot({ path: join(SHOTS, name) }) : Promise.resolve())
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
let pass = true
const has = (n, ok, d) => {
  console.log(`  [${ok ? '通过' : '失败'}] ${n}${ok ? '' : ' —— ' + (d ?? '')}`)
  if (!ok) pass = false
}

async function loadPlaywright() {
  try {
    return await import('playwright')
  } catch {}
  if (process.env.PLAYWRIGHT_MODULE) return import(pathToFileURL(process.env.PLAYWRIGHT_MODULE).href)
  const npx = join(homedir(), '.npm', '_npx')
  const cache = join(homedir(), '.cache', 'ms-playwright')
  const cached = existsSync(cache) ? readdirSync(cache) : []
  for (const d of readdirSync(npx)) {
    const core = join(npx, d, 'node_modules', 'playwright-core')
    const entry = join(npx, d, 'node_modules', 'playwright', 'index.mjs')
    try {
      const revs = JSON.parse(readFileSync(join(core, 'browsers.json'), 'utf8')).browsers
        .filter((b) => b.name.startsWith('chromium'))
        .map((b) => b.revision)
      if (!existsSync(entry) || !revs.some((r) => cached.some((c) => c.endsWith(`-${r}`)))) continue
      return await import(pathToFileURL(entry).href)
    } catch {}
  }
  throw new Error('找不到可用的 Playwright（需要与已缓存 Chromium 匹配的版本）')
}

// ---- server 生命周期（只用普通信号——worker 自成会话，不受影响）----
let server = null
async function portFree(port) {
  return new Promise((r) => {
    const s = connect(port, '127.0.0.1')
    s.once('connect', () => { s.destroy(); r(false) })
    s.once('error', () => r(true))
  })
}
async function startServer() {
  server = spawn(SERVER_BIN, {
    cwd: repoRoot,
    env: { ...process.env, LIMPET_ADDR: `127.0.0.1:${PORT}` },
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  server.stdout.resume()
  server.stderr.resume()
  const t0 = Date.now()
  while (Date.now() - t0 < 10000) {
    try {
      if ((await fetch(`${BASE}/api/term`)).ok) return
    } catch {}
    await sleep(100)
  }
  throw new Error('server 未在 10s 内就绪')
}
async function stopServer() {
  const s = server
  server = null
  if (!s) return
  s.kill('SIGTERM')
  await new Promise((r) => s.once('exit', r))
}

// ---- 可干预的 TCP 中继。模式只作用于「当前已建立的连接」，新连接一律正常转发
//      （页面的重连就是新连接）----
//   cut      切断现有连接（网络闪断）
//   stall    现有 WebSocket 连接上 server → 浏览器方向的字节全部丢弃（无声失联）
//   authdown 对新到的 GET /api/term 直接回 302（Access 登录态失效）
let mode = 'normal'
let wsOpens = 0 // 经中继建立的 WebSocket 连接数（页面的每次接入算一次）
const conns = new Set()
const relay = createServer((client) => {
  const upstream = connect(PORT, '127.0.0.1')
  const c = { client, upstream, stalled: false }
  conns.add(c)
  let head = Buffer.alloc(0)
  let peeked = false
  client.on('data', (d) => {
    if (!peeked) {
      head = Buffer.concat([head, d])
      if (!head.includes('\r\n\r\n')) return
      peeked = true
      const first = head.toString('latin1')
      if (mode === 'authdown' && /^GET \/api\/term(\?|\s)/.test(first)) {
        const body = 'redirect'
        client.end(
          `HTTP/1.1 302 Found\r\nLocation: https://login.example.invalid/authorize\r\nContent-Length: ${body.length}\r\nConnection: close\r\n\r\n${body}`,
        )
        return
      }
      c.ws = /^GET \/api\/ws\//.test(first)
      if (c.ws) wsOpens++
      if (mode === 'authdown' && !c.ws) {
        // 中继只检查每条 TCP 连接的第一个请求，模拟 Access 时避免复用 HTTP 连接。
        const headers = first.replace(/\r\nConnection:[^\r\n]*/i, '')
        head = Buffer.from(headers.replace('\r\n\r\n', '\r\nConnection: close\r\n\r\n'), 'latin1')
      }
      upstream.write(head)
      return
    }
    upstream.write(d)
  })
  upstream.on('data', (d) => {
    if (!c.stalled) client.write(d)
  })
  const drop = () => {
    conns.delete(c)
    client.destroy()
    upstream.destroy()
  }
  client.on('close', drop)
  upstream.on('close', drop)
  client.on('error', drop)
  upstream.on('error', drop)
})
const relayCut = () => {
  for (const c of conns) { c.client.destroy(); c.upstream.destroy() }
}
const relayStall = () => {
  for (const c of conns) if (c.ws) c.stalled = true
}

// ---- 浏览器侧小工具 ----
let page
const badgeSel = 'header [data-status]'
const badgeText = async () => (await page.locator(badgeSel).first().textContent())?.trim()
async function waitBadge(text, timeout = 15000) {
  await page.locator(badgeSel, { hasText: text }).first().waitFor({ state: 'visible', timeout })
  return badgeText()
}
async function createSession(name) {
  const r = await fetch(`${BASE}/api/term`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: testName(name) }),
  })
  if (!r.ok) throw new Error(`创建测试会话失败 HTTP ${r.status}`)
  const { id } = await r.json()
  if (!/^[0-9a-f]{16}$/.test(id)) throw new Error('未获得本轮测试会话 ID')
  ids.push(id)
  return id
}
async function openInPage(name) {
  await page.goto(`${ORIGIN}/`)
  await page.locator('li button').filter({ hasText: testName(name) }).first().click()
  await waitBadge('已连接')
}
async function typeLine(text) {
  await page.keyboard.type(text)
  await page.keyboard.press('Enter')
}
// 录制是每批输出即落盘的原始字节：命令行回显一次、执行结果一次。
async function recordedCount(id, marker, want, timeout = 6000) {
  const log = join(LIMPET_HOME, 'history', `${id}.log`)
  const t0 = Date.now()
  for (;;) {
    const n = existsSync(log) ? (readFileSync(log).toString('latin1').match(new RegExp(marker, 'g')) || []).length : 0
    if (n >= want || Date.now() - t0 > timeout) return n
    await sleep(200)
  }
}
async function seeBadge(text, within = 6000) {
  const t0 = Date.now()
  while (Date.now() - t0 < within) {
    if ((await badgeText()) === text) return true
    await sleep(100)
  }
  return false
}
const listHas = async (id) => ((await (await fetch(`${BASE}/api/term`)).json()).some((s) => s.id === id))

if (!(await portFree(PORT))) { console.error(`端口 ${PORT} 被占用：先停掉正在运行的 server`); process.exit(2) }
if (!(await portFree(PORT + 2))) { console.error(`端口 ${PORT + 2} 被占用`); process.exit(2) }
if (!existsSync(join(repoRoot, 'web', 'dist', 'index.html'))) { console.error('仓库里没有 web/dist：先在 web/ 下执行 pnpm build'); process.exit(2) }

const { chromium } = await loadPlaywright()
let browser
const ids = []
try {
  await startServer()
  relay.listen(PORT + 2, '127.0.0.1')
  try {
    browser = await chromium.launch({ headless: true })
  } catch {
    browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] })
  }
  const desktop = await (await browser.newContext({ viewport: { width: 1100, height: 700 } })).newPage()
  page = desktop

  // ---- S1 ----
  console.log('S1 接入与输入')
  const s1 = await createSession('重连-S1')
  await openInPage('重连-S1')
  has('打开会话后徽标为「已连接」', (await badgeText()) === '已连接', await badgeText())
  await typeLine('echo MARK-1')
  const mark1 = await recordedCount(s1, 'MARK-1', 2)
  has(`按键到达 Shell（MARK-1 出现 ${mark1} 处：逐字符回显 + 执行结果）`, mark1 >= 2)
  await shot(page, 'browser-connected.png')

  // ---- S2 ----
  console.log('S2 server 重启')
  await stopServer()
  has('server 停止后页面进入「重连中」', await seeBadge('重连中'))
  await sleep(2500)
  has('server 停止期间保持「重连中」', (await badgeText()) === '重连中', await badgeText())
  await startServer()
  await waitBadge('已连接', 20000)
  has('server 回来后自动接回（无需人工干预）', true)
  await sleep(2000)
  has(`重连没有向 Shell 重放输入（MARK-1 仍是 ${mark1} 处）`, (await recordedCount(s1, 'MARK-1', 0)) === mark1)
  await typeLine('echo MARK-2')
  has('接回后可以继续输入（MARK-2 到达 Shell）', (await recordedCount(s1, 'MARK-2', 2)) >= 2)

  // ---- S3 ----
  console.log('S3 网络闪断与旧核对结果')
  let releaseCheck
  const holdCheck = new Promise((resolve) => { releaseCheck = resolve })
  const listURL = `${ORIGIN}/api/term`
  await page.route(listURL, async (route) => {
    await holdCheck
    await route.fulfill({ json: [] })
  }, { times: 1 })
  const checking = page.waitForRequest(listURL, { timeout: 8000 })
  try {
    relayCut()
    await checking
    has('会话核对仍挂起时徽标已显示「重连中」', (await badgeText()) === '重连中')
    await page.evaluate(() => window.dispatchEvent(new Event('online')))
    await waitBadge('已连接')
    has('online 可以在核对返回前接回会话', true)
    const oldResponse = page.waitForResponse(listURL, { timeout: 5000 })
    releaseCheck()
    await oldResponse
    await sleep(250)
    has('旧核对返回空列表后新连接仍有效', (await badgeText()) === '已连接')
  } finally {
    releaseCheck()
  }

  // ---- S4 ----
  console.log('S4 登录失效')
  mode = 'authdown'
  relayCut()
  await waitBadge('登录已失效')
  has('核对请求被重定向 → 「登录已失效」', true)
  has('终端区出现「刷新页面」按钮', await page.locator('main').getByRole('button', { name: '刷新页面' }).isVisible())
  has('侧栏同时显示「刷新页面」按钮', await page.getByRole('complementary', { name: '会话列表' }).getByRole('button', { name: '刷新页面' }).isVisible())
  await page.locator('main').getByRole('button', { name: '刷新页面' }).click()
  await page.getByRole('heading', { name: 'Limpet' }).waitFor({ state: 'visible' })
  await page.getByText('登录已失效', { exact: true }).waitFor()
  has('列表能识别 Access 重定向并提供刷新按钮', await page.getByRole('button', { name: '刷新页面' }).isVisible())
  mode = 'normal'
  await page.getByRole('button', { name: '刷新页面' }).click()
  await page.getByRole('button', { name: new RegExp(testName('重连-S1')) }).waitFor({ state: 'visible' })
  has('重新登录后刷新能恢复列表', true)

  // ---- S5 ----
  console.log('S5 无声失联（约 1 分钟）')
  await openInPage('重连-S1')
  const opens = wsOpens
  const badgeTransitions = await page.locator(badgeSel).first().evaluateHandle((badge) => {
    const statuses = new Set([badge.getAttribute('data-status')])
    const observer = new MutationObserver((records) => {
      for (const change of records) statuses.add(change.oldValue)
      statuses.add(badge.getAttribute('data-status'))
    })
    observer.observe(badge, { attributeFilter: ['data-status'], attributeOldValue: true })
    return { statuses, observer }
  })
  let decide = 0
  relayStall()
  const t5 = Date.now()
  // 判定时刻以「页面发起了新的接入」为准：浏览器自己不会因为收不到数据而触发 close，
  // 是前端看门狗在 60s 没收到任何消息后主动关闭并重连的。徽标过渡可能短于轮询间隔。
  while (Date.now() - t5 < 85000) {
    if (!decide && wsOpens > opens) decide = (Date.now() - t5) / 1000
    const t = await badgeText()
    if (t === '已连接' && decide) break
    await sleep(50)
  }
  const back = (Date.now() - t5) / 1000
  const sawReconnecting = await badgeTransitions.evaluate(({ statuses, observer }) => {
    observer.disconnect()
    return statuses.has('reconnecting')
  })
  await badgeTransitions.dispose()
  has(`没有任何帧到达时 ${decide.toFixed(1)}s 自行判定断开（≤ 65s）`, decide > 0 && decide <= 65)
  has('判定期间徽标显示「重连中」', sawReconnecting)
  has(`随后自动接回（共 ${back.toFixed(1)}s）`, back <= 80 && (await badgeText()) === '已连接')
  await typeLine('echo MARK-5')
  has('接回后可以继续输入（MARK-5 到达 Shell）', (await recordedCount(s1, 'MARK-5', 2)) >= 2)

  // ---- S6 ----
  console.log('S6 Shell 内 exit')
  await typeLine('exit')
  await waitBadge('会话已结束')
  has('exit → 「会话已结束」', true)
  await sleep(6000)
  has('6 秒后仍是「会话已结束」（不再重连）', (await badgeText()) === '会话已结束', await badgeText())
  has('列表里已无该会话', !(await listHas(s1)))

  // ---- S7 ----
  console.log('S7 列表里关闭会话')
  const s7 = await createSession('重连-S7')
  await openInPage('重连-S7')
  await fetch(`${BASE}/api/term/${s7}/close`, { method: 'POST' })
  await waitBadge('会话已结束')
  has('会话被关闭 → 页面「会话已结束」', true)

  // ---- S8 ----
  console.log('S8 worker 被 kill -9')
  const s8 = await createSession('重连-S8')
  await openInPage('重连-S8')
  const binaryName = basename(SERVER_BIN).replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const binaryPattern = binaryName.replace(/[a-zA-Z]/, (letter) => `[${letter}]`)
  const wpid = execFileSync('pgrep', ['-f', '--', `(^|/)${binaryPattern} worker ${s8}$`], { encoding: 'utf8' }).trim()
  if (!/^[1-9][0-9]*$/.test(wpid)) throw new Error('预期找到唯一的测试 worker PID')
  process.kill(Number(wpid), 'SIGKILL')
  await waitBadge('会话已结束')
  has('worker 消失 → 核对后进入「会话已结束」', true)

  // ---- S9 ----
  console.log('S9 移动视口 375')
  const mctx = await browser.newContext({ viewport: { width: 375, height: 812 }, hasTouch: true, isMobile: true })
  const mpage = await mctx.newPage()
  page = mpage
  const s9 = await createSession('重连-S9')
  await openInPage('重连-S9')
  has('移动视口下徽标为「已连接」', (await badgeText()) === '已连接', await badgeText())
  const ctrl = page.getByRole('button', { name: 'Ctrl', exact: true })
  const ctrlC = page.getByRole('button', { name: 'Ctrl+C', exact: true })
  await ctrl.click()
  has('粘性 Ctrl 点亮时 Ctrl+C 保持普通按键语义',
    (await ctrl.getAttribute('aria-pressed')) === 'true' &&
    (await ctrlC.getAttribute('aria-pressed')) === null)
  await ctrl.click()
  await typeLine('echo MARK-9')
  has('移动视口下按键到达 Shell', (await recordedCount(s9, 'MARK-9', 2)) >= 2)
  await shot(mpage, 'browser-mobile.png')
  await mctx.close()

  // ---- S10 ----
  console.log('S10 列表表单与关闭中状态')
  page = desktop
  const s10 = await createSession('关闭中会话')
  await page.goto(ORIGIN)
  const row = page.locator('li').filter({ hasText: testName('关闭中会话') })
  await row.waitFor({ state: 'visible' })
  await page.getByRole('complementary', { name: '会话列表' }).getByRole('button', { name: '新建', exact: true }).click()
  has('创建输入框提示留空自动生成', await page.getByPlaceholder('会话名（留空自动生成）').isVisible())
  await page.getByRole('button', { name: '取消' }).click()
  await row.locator('[title="重命名"]').click()
  has('重命名输入框提示输入新的会话名', await page.getByPlaceholder('输入新的会话名').isVisible())
  await page.getByRole('button', { name: '取消' }).click()

  const closeURL = `${ORIGIN}/api/term/${s10}/close`
  let releaseClose
  const holdClose = new Promise((resolve) => { releaseClose = resolve })
  await page.route(closeURL, async (route) => {
    await holdClose
    await route.continue()
  }, { times: 1 })
  const closing = page.waitForRequest(closeURL, { timeout: 8000 })
  try {
    await row.locator('[title="关闭"]').click()
    const dialog = page.getByRole('dialog', { name: '关闭会话' })
    has('关闭确认初始焦点在取消按钮', await dialog.getByRole('button', { name: '取消' }).evaluate((el) => el === document.activeElement))
    await dialog.getByRole('button', { name: '确认关闭' }).click()
    await closing
    has('关闭期间禁用对应会话的打开、重命名与关闭按钮',
      await row.getByRole('button', { name: new RegExp(testName('关闭中会话')) }).isDisabled() &&
      await row.getByRole('button', { name: '重命名' }).isDisabled() &&
      await row.locator('[title="关闭"]').isDisabled() &&
      await page.getByRole('button', { name: new RegExp(testName('重连-S9')) }).isEnabled())
  } finally {
    releaseClose()
  }
  await row.waitFor({ state: 'detached' })
  has('关闭完成后会话从列表移除', true)
} catch (e) {
  has(`执行中断：${e instanceof Error ? e.message.split('\n')[0] : e}`, false)
} finally {
  for (const id of ids) await fetch(`${BASE}/api/term/${id}/close`, { method: 'POST' }).catch(() => {})
  await sleep(3500)
  await browser?.close().catch(() => {})
  relay.close()
  await stopServer().catch(() => {})
}
console.log(pass ? '=> 通过' : '=> 失败')
process.exit(pass ? 0 : 1)
