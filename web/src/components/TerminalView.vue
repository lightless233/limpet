<script lang="ts">
// 模块级：WebGL2 探测结果按浏览器会话缓存一次。
let rendererProbe: 'dom' | 'webgl' | null = null

// 挂载 <Terminal> 前探测 webgl 渲染器是否可用（该 prop 仅在挂载时生效）：
// 需要 OffscreenCanvas（含 transferToImageBitmap）与可用的 webgl2 上下文；
// 软件渲染器（RDP/虚拟机里的 SwiftShader、llvmpipe 等）跑 webgl 反而更慢，
// 必须回退 dom。任何异常一律静默回退 dom。
const SOFTWARE_RENDERER_PATTERNS = ['swiftshader', 'llvmpipe', 'softpipe', 'software', 'microsoft basic render']

function detectRenderer(): 'dom' | 'webgl' {
  if (rendererProbe) return rendererProbe
  rendererProbe = 'dom'
  try {
    if (typeof OffscreenCanvas === 'undefined' || !OffscreenCanvas.prototype.transferToImageBitmap) return 'dom'
    // failIfMajorPerformanceCaveat 让软件实现（SwiftShader/llvmpipe）直接创建失败：
    // 隐私策略可能屏蔽 WEBGL_debug_renderer_info，仅靠字符串会漏判。
    const gl = new OffscreenCanvas(1, 1).getContext('webgl2', { failIfMajorPerformanceCaveat: true })
    if (!gl) return 'dom'
    let names = String(gl.getParameter(gl.RENDERER) ?? '')
    const debug = gl.getExtension('WEBGL_debug_renderer_info')
    if (debug) names += ' ' + String(gl.getParameter(debug.UNMASKED_RENDERER_WEBGL) ?? '')
    // 立即释放探测用的 WebGL 上下文，不占用浏览器配额。
    gl.getExtension('WEBGL_lose_context')?.loseContext()
    const lowered = names.toLowerCase()
    if (!SOFTWARE_RENDERER_PATTERNS.some((p) => lowered.includes(p))) rendererProbe = 'webgl'
  } catch { /* 探测失败一律回退 dom */ }
  return rendererProbe
}
</script>

<script setup lang="ts">
// 终端视图：wterm(ghostty core) ↔ WebSocket ↔ Go(worker PTY)。
// 帧约定：二进制帧 = 终端 I/O；文本帧 = 控制 JSON（后端→ready/size/ping，前端→resize/pong）。
// 接入由调用方给定的会话 ID 直接进行（会话已在列表中创建）。
// 断线自动重连（spec 005）：网络抖动、切网、锁屏恢复、server 重启之后自动接回原会话。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, useTemplateRef } from 'vue'
import { Terminal, type WTerm } from '@wterm/vue'
import { GhosttyCore } from '@wterm/ghostty'
import '@wterm/vue/css'
import type { SessionItem } from '../sessions'
import AppIcon from './AppIcon.vue'

const props = defineProps<{
  sessionId: string; sessionName: string; sessions: SessionItem[]
  sessionsLoading: boolean; sessionsError: string; authExpired: boolean
}>()
const emit = defineEmits<{
  back: []; openSession: [id: string, name: string]; refreshSessions: []; sessionAuthExpired: []
}>()

// server 每 20s 发一帧 ping；超过这个时限没收到任何消息即判定连接已死（与服务端的 60s 判定对称）。
const PING_TIMEOUT_MS = 60_000
const BACKOFF_START_MS = 1000
const BACKOFF_MAX_MS = 30_000

// 渲染器：webgl 仅在硬件加速可用时启用（探测结果模块级缓存）。
const terminalRenderer = detectRenderer()

const term = useTemplateRef('term')
const rail = useTemplateRef('rail')
const composerInput = useTemplateRef('composerInput')
const core = ref<GhosttyCore | null>(null)
// 已加载但尚未晋升到 core 的实例：ready 帧前 WebSocket 断掉/重试/卸载时，
// 这个孤儿不会被 <Terminal> 引用，必须显式释放（WTerm.destroy 不释放 core）。
let pendingCore: GhosttyCore | null = null
// fatalError：core 加载或 Terminal 初始化失败，终端组件无法存在，
// 以全屏错误替代终端；重新连接会重建 core 并重新挂载组件。
const fatalError = ref('')

// reconnecting：自动重连进行中（含退避等待）。ended / authExpired 是终态，不再重连。
type Status = 'connecting' | 'reconnecting' | 'connected' | 'ended' | 'authExpired' | 'error'
const status = ref<Status>('connecting')

const encoder = new TextEncoder()
let ws: WebSocket | null = null
// 每次接入重新挂载 Terminal，等该实例的 @ready 再交付输出。
let termReady = false
type Dimensions = { cols: number; rows: number }
// Vue 终端挂载前收到的尺寸与 VT 字节按同一顺序重放。
const pending: (Uint8Array | Dimensions)[] = []
const lockPreference = ref(localStorage.getItem('limpet.lock-width') === 'true')
const supportsLockWidth = ref<boolean | null>(null)
const lockedWidth = ref(false)
const remoteSize = ref<Dimensions>()
const defaultFontSize = matchMedia('(min-width: 768px)').matches ? 13 : 11
const storedFontSize = Number(localStorage.getItem('limpet.font-size'))
const fontSize = ref(Number.isInteger(storedFontSize) && storedFontSize >= 10 && storedFontSize <= 18 ? storedFontSize : defaultFontSize)
const terminalStyle = computed(() => ({
  '--limpet-font-size': fontSize.value + 'px',
  '--limpet-row-height': Math.ceil(fontSize.value * 1.25) + 'px',
  '--limpet-remote-cols': remoteSize.value?.cols,
}))
const viewportHeight = ref<number>()
const viewportTop = ref(0)
const railWidth = ref(0)
let stopRail: (() => void) | undefined

// 重连状态。gen 是接入代数：connect() 中途有 await，期间若发起了新的接入，旧的那次自行作废。
let gen = 0
let waitingToReconnect = false
let reconnectDelay = 0
let reconnectTimer = 0
let watchdog = 0
let lastMsgAt = 0
let unmounted = false

onMounted(() => {
  window.addEventListener('online', reconnectNow)
  window.addEventListener('focus', requestImmediateRender)
  document.addEventListener('visibilitychange', reconnectNow)
  window.visualViewport?.addEventListener('resize', syncViewport)
  window.visualViewport?.addEventListener('scroll', syncViewport)
  syncViewport()
  watchdog = window.setInterval(checkLiveness, 1000)
  void connect()
})

onBeforeUnmount(() => {
  unmounted = true
  window.removeEventListener('online', reconnectNow)
  window.removeEventListener('focus', requestImmediateRender)
  document.removeEventListener('visibilitychange', reconnectNow)
  window.visualViewport?.removeEventListener('resize', syncViewport)
  window.visualViewport?.removeEventListener('scroll', syncViewport)
  clearTimeout(reconnectTimer)
  clearInterval(watchdog)
  detachSocket()
  resetThrottle()
  pending.length = 0
  // <Terminal> 在本组件卸载流程里才执行 WTerm.destroy()（它不会释放 core）；
  // releaseCores 经 nextTick 等卸载完成后再释放最后一轮 core 的 WASM 内存。
  releaseCores()
})

function detachSocket() {
  stopRail?.()
  stopRail = undefined
  if (!ws) return
  // 先摘回调再关闭，避免旧连接的 onclose 污染新一轮连接的状态。
  ws.onopen = ws.onmessage = ws.onclose = null
  ws.close()
  ws = null
}

// releaseCores 释放上一轮的 core。pendingCore 无人引用，同步释放即可；
// core.value 仍被 <Terminal>（WTerm）引用：先置 null 让其卸载，再在
// nextTick（WTerm.destroy() 之后）释放 WASM 内存（dispose 幂等，全方法有守卫）。
function releaseCores() {
  pendingCore?.dispose()
  pendingCore = null
  const prev = core.value
  core.value = null
  if (prev) void nextTick(() => prev.dispose())
}

async function connect() {
  const my = ++gen
  clearTimeout(reconnectTimer)
  reconnectTimer = 0
  waitingToReconnect = false
  detachSocket()
  // 旧连接的残余输出与节流定时器全部作废，不得串进新实例。
  resetThrottle()
  pending.length = 0
  status.value = my === 1 ? 'connecting' : 'reconnecting'
  // 每次接入都重建 core 并重新挂载终端组件，让本次接入拥有干净的
  // 屏幕状态。组件重挂载会重新触发 @ready，termReady 随之复位。
  releaseCores()
  termReady = false
  fatalError.value = ''
  supportsLockWidth.value = null
  lockedWidth.value = false
  remoteSize.value = undefined
  railWidth.value = 0
  lastMsgAt = 0

  let loaded: GhosttyCore
  try {
    loaded = await GhosttyCore.load()
    if (unmounted || my !== gen) {
      // 这次接入已被更新的接入或卸载作废：释放刚加载的 WASM 内存。
      loaded.dispose()
      return
    }
    // 晋升推迟到 ready 帧：在此之前断线/重试/卸载时由 releaseCores 兜底释放。
    pendingCore = loaded
  } catch (e) {
    if (unmounted || my !== gen) return
    fatalError.value = e instanceof Error ? e.message : String(e)
    status.value = 'error'
    return
  }
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const sock = new WebSocket(`${proto}//${location.host}/api/ws/term/${encodeURIComponent(props.sessionId)}`)
  ws = sock
  sock.binaryType = 'arraybuffer'
  // 收到 ready 帧才算「已连接」并开放输入。
  sock.onopen = () => {
    if (ws !== sock) return
    lastMsgAt = Date.now()
    if (lockPreference.value) sock.send('{"type":"resize","cols":0,"rows":0}')
  }
  sock.onmessage = (e) => {
    if (ws !== sock) return
    lastMsgAt = Date.now()
    if (typeof e.data === 'string') {
      // 文本帧 = 控制 JSON。
      try {
        const ctl = JSON.parse(e.data)
        if (ctl.type === 'ready' && !core.value) {
          reconnectDelay = 0
          remoteSize.value = parseDimensions(ctl)
          supportsLockWidth.value = remoteSize.value !== undefined
          lockedWidth.value = lockPreference.value && supportsLockWidth.value
          pendingCore = null
          core.value = loaded
        } else if (ctl.type === 'size' && lockedWidth.value) {
          const size = parseDimensions(ctl)
          if (size) deliver(size)
        } else if (ctl.type === 'ping' && sock.readyState === WebSocket.OPEN) {
          sock.send('{"type":"pong"}')
        }
      } catch { /* 忽略无法解析的控制帧 */ }
      return
    }
    deliver(new Uint8Array(e.data as ArrayBuffer))
  }
  sock.onclose = (e) => {
    // 已被替换或主动关闭的旧连接不算断线。
    if (ws !== sock) return
    ws = null
    void onDisconnected(e.code)
  }
}

// 断线分流：1000 是会话结束；其余（1006 网络断开或握手失败、1011 worker 连接异常、
// 4001 慢连接被丢弃、4002 心跳超时）先核对会话是否还在，再决定重连还是进入终态。
async function onDisconnected(code: number) {
  if (unmounted) return
  // ready 帧前断线：尚未晋升的 core 成孤儿，先释放（core.value 里的实例仍被
  // <Terminal> 展示当前画面，交给后续接入或卸载时统一释放）。
  pendingCore?.dispose()
  pendingCore = null
  if (code === 1000) {
    status.value = 'ended'
    emit('refreshSessions')
    return
  }
  status.value = 'reconnecting'
  waitingToReconnect = true
  const my = gen
  const verdict = await checkSession()
  if (unmounted || my !== gen) return
  if (verdict === 'gone' || verdict === 'auth') {
    waitingToReconnect = false
    status.value = verdict === 'gone' ? 'ended' : 'authExpired'
    if (verdict === 'auth') emit('sessionAuthExpired')
    else emit('refreshSessions')
    return
  }
  scheduleReconnect()
}

// 核对会话是否还在。redirect: 'manual' 是为了认出 Access 登录态失效——
// 那时请求被重定向到登录页，跟过去只会得到一个跨域错误，分不清原因。
async function checkSession(): Promise<'alive' | 'gone' | 'auth' | 'unknown'> {
  try {
    const res = await fetch('/api/term', { redirect: 'manual', cache: 'no-store', signal: AbortSignal.timeout(8000) })
    if (res.type === 'opaqueredirect') return 'auth'
    if (!res.ok) return 'unknown'
    const list: { id: string }[] = await res.json()
    return list.some((s) => s.id === props.sessionId) ? 'alive' : 'gone'
  } catch {
    return 'unknown'
  }
}

// 首次立即，之后 1s、2s、4s… 封顶 30s；收到 ready 复位。
function scheduleReconnect() {
  const delay = reconnectDelay
  reconnectDelay = delay === 0 ? BACKOFF_START_MS : Math.min(delay * 2, BACKOFF_MAX_MS)
  if (delay === 0) {
    void connect()
    return
  }
  reconnectTimer = window.setTimeout(() => {
    reconnectTimer = 0
    void connect()
  }, delay)
}

// 网络恢复或页面回到前台时，可以跳过尚未完成的核对或退避。
function reconnectNow() {
  if (unmounted || document.visibilityState === 'hidden' || !waitingToReconnect) return
  reconnectDelay = Math.max(reconnectDelay, BACKOFF_START_MS)
  void connect()
}

// 连接无声死亡时浏览器不会触发 close（手机切网的常态）。server 每 20s 发 ping，
// 60s 没收到任何消息就当作已死：主动关闭并重连。
function checkLiveness() {
  if (!ws || status.value !== 'connected') return
  if (Date.now() - lastMsgAt <= PING_TIMEOUT_MS) return
  scheduleReconnect()
}

function parseDimensions(ctl: Dimensions): Dimensions | undefined {
  const { cols, rows } = ctl
  if (Number.isInteger(cols) && cols >= 2 && cols <= 500 && Number.isInteger(rows) && rows >= 2 && rows <= 200) {
    return { cols, rows }
  }
}

// ---- 输出合批与渲染节流 ----
// WS 的二进制分片先在轻量队列里合并成一个 Uint8Array，再一次性喂给 write()。
// setRenderingPaused 只推迟绘制：字节始终持续喂入，Ghostty WASM 状态、终端
// 查询回复与 scrollback 实时准确，不积压也不丢帧。
const THROTTLE_BURST_BYTES = 32 * 1024 // 每秒输出超过该字节数视为高频爆发
const THROTTLE_SLOW_FRAME_MS = 12 // 单次喂入耗时超过该值视为慢帧
const THROTTLE_IDLE_UNPAUSE_MS = 80 // 输出停顿这么久立即解除暂停刷出画面
const outbox: Uint8Array[] = []
let outboxTimer = 0
let lastOutputAt = 0
let burstBytes = 0
let burstWindowAt = 0
let lastFrameCost = 0
let renderPaused = false
let pauseTimer = 0
let idleTimer = 0
let unpauseRaf = 0

function setRenderingPaused(paused: boolean) {
  term.value?.instance?.setRenderingPaused(paused)
}

function scheduleOutboxFlush() {
  if (outboxTimer) return
  outboxTimer = window.setTimeout(flushOutbox, 0)
}

function flushOutbox() {
  if (outboxTimer) {
    clearTimeout(outboxTimer)
    outboxTimer = 0
  }
  if (!outbox.length) return
  let total = 0
  for (const chunk of outbox) total += chunk.length
  const merged = new Uint8Array(total)
  let at = 0
  for (const chunk of outbox) {
    merged.set(chunk, at)
    at += chunk.length
  }
  outbox.length = 0
  const started = performance.now()
  term.value?.write(merged)
  const cost = performance.now() - started
  lastFrameCost = lastFrameCost === 0 ? cost : lastFrameCost * 0.6 + cost * 0.4
  const now = performance.now()
  if (now - burstWindowAt >= 1000) {
    burstWindowAt = now
    burstBytes = 0
  }
  burstBytes += merged.length
  if (renderPaused) {
    // 输出仍在持续：顺延「停顿解除」；节拍解除照常到点刷出。
    clearTimeout(idleTimer)
    idleTimer = window.setTimeout(endRenderPause, THROTTLE_IDLE_UNPAUSE_MS)
  } else {
    maybeThrottle()
  }
}

// 爆发判定：输出流正在进行中，且吞吐超阈值或上一帧偏慢。
function shouldThrottle(): boolean {
  if (document.visibilityState !== 'visible') return false
  if (performance.now() - lastOutputAt > 40) return false
  return burstBytes >= THROTTLE_BURST_BYTES || lastFrameCost > THROTTLE_SLOW_FRAME_MS
}

// 目标重绘间隔：聚焦 + WebGL 33–50ms；DOM 渲染器或慢帧 80–200ms；
// 可见但未聚焦的双开窗口在爆发期压到 150–250ms。压力越大越靠区间后段。
function throttleInterval(): number {
  let lo = 33
  let hi = 50
  if (terminalRenderer !== 'webgl' || lastFrameCost > THROTTLE_SLOW_FRAME_MS) {
    lo = 80
    hi = 200
  }
  if (document.visibilityState === 'visible' && !document.hasFocus()) {
    lo = 150
    hi = 250
  }
  const pressure = Math.min(1, Math.max(burstBytes / (THROTTLE_BURST_BYTES * 8), lastFrameCost / 30))
  return Math.round(lo + (hi - lo) * pressure)
}

function maybeThrottle() {
  // unpauseRaf 非零表示上一帧的绘制尚未画完：此时 setRenderingPaused(true)
  // 会取消刚排好的 WTerm._flushRenders，把爆发期放行的帧整个误杀。
  if (renderPaused || unpauseRaf || !shouldThrottle()) return
  renderPaused = true
  setRenderingPaused(true)
  pauseTimer = window.setTimeout(endRenderPause, throttleInterval())
  idleTimer = window.setTimeout(endRenderPause, THROTTLE_IDLE_UNPAUSE_MS)
}

function endRenderPause() {
  clearTimeout(pauseTimer)
  clearTimeout(idleTimer)
  pauseTimer = idleTimer = 0
  if (!renderPaused) return
  renderPaused = false
  setRenderingPaused(false)
  armUnpauseDecision()
}

// 解除暂停后安排「下一拍」决策：WTerm 的绘制 rAF 先于本回调注册，回调执行时
// 这一帧已画完。unpauseRaf 非零期间 maybeThrottle 一律不再暂停。
function armUnpauseDecision() {
  if (unpauseRaf) return
  unpauseRaf = requestAnimationFrame(() => {
    unpauseRaf = 0
    if (unmounted) return
    maybeThrottle()
  })
}

// 用户输入或窗口重新聚焦：立即解除暂停刷出最新画面，绝不留在 paused 状态；
// 并保证至少完整画完这一帧，之后才由 maybeThrottle 决定是否继续节流。
function requestImmediateRender() {
  clearTimeout(pauseTimer)
  clearTimeout(idleTimer)
  pauseTimer = idleTimer = 0
  if (renderPaused) {
    renderPaused = false
    setRenderingPaused(false)
  }
  armUnpauseDecision()
}

// 断线、重连或卸载：清空缓存队列与所有定时器，旧连接的残余字节绝不串进新实例。
function resetThrottle() {
  cancelAnimationFrame(unpauseRaf)
  clearTimeout(pauseTimer)
  clearTimeout(idleTimer)
  clearTimeout(outboxTimer)
  unpauseRaf = pauseTimer = idleTimer = outboxTimer = 0
  outbox.length = 0
  if (renderPaused) {
    renderPaused = false
    setRenderingPaused(false)
  }
  lastOutputAt = 0
  burstBytes = 0
  burstWindowAt = 0
  lastFrameCost = 0
}

function deliver(event: Uint8Array | Dimensions) {
  if (!termReady) {
    pending.push(event)
  } else if (event instanceof Uint8Array) {
    lastOutputAt = performance.now()
    outbox.push(event)
    scheduleOutboxFlush()
  } else {
    // 尺寸帧严格保序：先同步刷空此前缓存的输出字节再 resize，
    // 保证尺寸不会越过前后的 VT 字节。props 同时保留当前几何。
    flushOutbox()
    remoteSize.value = event
    term.value?.resize(event.cols, event.rows)
  }
}

// 输入闸门：WS 打开 + 已收到 ready。接入中与重连中的按键直接丢弃，不缓冲。
function sendInput(bytes: Uint8Array): boolean {
  const sock = ws
  if (sock?.readyState !== WebSocket.OPEN || status.value !== 'connected') return false
  const CHUNK = 64 * 1024
  try {
    for (let i = 0; i < bytes.length; i += CHUNK) {
      if (sock.readyState !== WebSocket.OPEN) return false
      sock.send(bytes.slice(i, i + CHUNK))
    }
    return sock.readyState === WebSocket.OPEN
  } catch {
    return false
  }
}

function onData(chunk: string) {
  requestImmediateRender()
  if (ctrlSticky.value) {
    ctrlSticky.value = false
    if (chunk.length === 1) {
      const code = chunk.toUpperCase().charCodeAt(0)
      if (code >= 64 && code <= 95) {
        // @A-Z[\]^_ → 控制字符 0x00-0x1F
        sendInput(encoder.encode(String.fromCharCode(code & 0x1f)))
        return
      }
    }
  }
  sendInput(encoder.encode(chunk))
}

// 终端程序开启鼠标跟踪后的原始 X10 报告。
function onBinary(bytes: Uint8Array) {
  requestImmediateRender()
  sendInput(new Uint8Array(bytes))
}

function onResize(cols: number, rows: number) {
  if (!lockedWidth.value && ws?.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: 'resize', cols, rows }))
  }
}

function onReady(wt: WTerm) {
  if (ws?.readyState !== WebSocket.OPEN || !core.value) return
  onResize(wt.cols, wt.rows)
  termReady = true
  status.value = 'connected'
  for (const event of pending) deliver(event)
  pending.length = 0
  if (lockedWidth.value) observeRail(wt.element)
  emit('refreshSessions')
  const my = gen
  void nextTick(() => {
    const focus = document.activeElement
    if (my === gen && status.value === 'connected' && focus !== composerInput.value && !focus?.closest('dialog[open]')) {
      term.value?.focus()
    }
  })
}

// WTerm 初始化失败（wasm 加载/实例化问题）：终端不可用，转为 fatalError；
// 下次重连会重建 core 并重新挂载组件。
function onTerminalError(e: unknown) {
  gen++
  waitingToReconnect = false
  clearTimeout(reconnectTimer)
  reconnectTimer = 0
  detachSocket()
  resetThrottle()
  pending.length = 0
  releaseCores()
  fatalError.value = String(e)
  status.value = 'error'
}

// 移动按键栏：以终端输入形式发送常用按键（charter 验收 3）。
// 用 click 发送：横滑滚动按键栏的触摸手势不会误发按键；
// 发送后把焦点还给终端。Ctrl 为粘性修饰键（spec 001）：
// 点亮后下一个字母输入组合成控制字符，组合完成自动熄灭。
const KEY_DEFS: { label: string; bytes: string }[] = [
  { label: 'Esc', bytes: '\x1b' },
  { label: 'Ctrl+C', bytes: '\x03' },
  { label: 'Tab', bytes: '\t' },
  { label: '↑', bytes: '\x1b[A' },
  { label: '↓', bytes: '\x1b[B' },
  { label: '←', bytes: '\x1b[D' },
  { label: '→', bytes: '\x1b[C' },
  { label: 'Enter', bytes: '\r' },
]

const ctrlSticky = ref(false)

function sendKey(bytes: string) {
  requestImmediateRender()
  sendInput(encoder.encode(bytes))
  term.value?.focus()
}

function toggleCtrl() {
  ctrlSticky.value = !ctrlSticky.value
  term.value?.focus()
}

function syncViewport() {
  const viewport = window.visualViewport
  const keyboardResized = viewport && Math.abs(viewport.scale - 1) < 0.01 && viewport.height < window.innerHeight - 1
  viewportHeight.value = keyboardResized ? viewport.height : undefined
  viewportTop.value = keyboardResized ? viewport.offsetTop : 0
}

function observeRail(el: HTMLElement) {
  const syncScroll = () => {
    if (rail.value && rail.value.scrollLeft !== el.scrollLeft) rail.value.scrollLeft = el.scrollLeft
  }
  let lastHeight = el.clientHeight
  let following = el.scrollHeight - el.scrollTop - lastHeight < 5
  let lastGridWidth = 0
  const onScroll = () => {
    if (el.clientHeight === lastHeight) following = el.scrollHeight - el.scrollTop - lastHeight < 5
    syncScroll()
  }
  const observer = new ResizeObserver((entries) => {
    // 网格高度随滚动历史每行增长属常态；仅容器尺寸或网格宽度真正变化时才
    // 重新测量同步，忽略纯高度变化，避免滚动时每行触发一次同步布局读取。
    let relevant = false
    for (const entry of entries) {
      if (entry.target === el) {
        relevant = true
      } else if (entry.contentRect.width !== lastGridWidth) {
        lastGridWidth = entry.contentRect.width
        relevant = true
      }
    }
    if (!relevant) return
    if (el.clientHeight !== lastHeight) {
      lastHeight = el.clientHeight
      if (following) el.scrollTop = el.scrollHeight
    }
    const overflow = el.scrollWidth - el.clientWidth
    railWidth.value = overflow > 1 ? el.offsetWidth + overflow : 0
    void nextTick(syncScroll)
  })
  observer.observe(el)
  const grid = el.querySelector('.term-grid')
  if (grid) observer.observe(grid)
  el.addEventListener('scroll', onScroll, { passive: true })
  stopRail = () => {
    observer.disconnect()
    el.removeEventListener('scroll', onScroll)
  }
}

function panTerminal() {
  const el = term.value?.instance?.element
  if (el && rail.value && el.scrollLeft !== rail.value.scrollLeft) el.scrollLeft = rail.value.scrollLeft
}

function scrollToBottom() {
  const el = term.value?.instance?.element
  if (el) el.scrollTop = el.scrollHeight
}

function toggleLockWidth() {
  if (!supportsLockWidth.value || status.value !== 'connected') return
  lockPreference.value = !lockedWidth.value
  localStorage.setItem('limpet.lock-width', String(lockPreference.value))
  void connect()
}

function setFontSize(size?: number) {
  fontSize.value = size ?? defaultFontSize
  if (size === undefined) localStorage.removeItem('limpet.font-size')
  else localStorage.setItem('limpet.font-size', String(size))
}

const composerOpen = ref(false)
const draft = ref('')
function toggleComposer() {
  composerOpen.value = !composerOpen.value
  void nextTick(() => {
    if (composerOpen.value) composerInput.value?.focus({ preventScroll: true })
    else term.value?.focus()
  })
}

function sendDraft(enter: boolean) {
  requestImmediateRender()
  if (sendInput(encoder.encode(draft.value + (enter ? '\r' : '')))) {
    draft.value = ''
    composerInput.value?.focus({ preventScroll: true })
  }
}

function composerEnter(e: KeyboardEvent) {
  if (e.isComposing || e.keyCode === 229) {
    e.preventDefault()
    return
  }
  if (e.shiftKey) return
  e.preventDefault()
  composerInput.value?.form?.requestSubmit()
}

function refreshQuickSwitch(e: ToggleEvent) {
  if (e.newState === 'open') emit('refreshSessions')
}

const statusText = {
  connecting: '接入中',
  reconnecting: '重连中',
  connected: '已连接',
  ended: '会话已结束',
  authExpired: '登录已失效',
  error: '错误',
} as const

function reloadPage() {
  location.reload()
}
</script>

<template>
  <main class="terminal-page flex min-h-0 min-w-0 flex-col bg-[var(--limpet-canvas)]"
    :style="{ height: viewportHeight === undefined ? '100%' : viewportHeight + 'px', marginTop: viewportTop + 'px' }">
    <header class="terminal-header flex shrink-0 items-center gap-1 border-b border-[var(--limpet-line)] bg-[var(--limpet-surface)] text-sm">
      <button class="control shrink-0" aria-label="返回会话列表" title="返回会话列表" @click="emit('back')">
        <AppIcon name="back" />
      </button>
      <button class="control min-w-0 flex-1 justify-start bg-transparent text-left hover:bg-[var(--limpet-pressed)] active:bg-[var(--limpet-pressed)]" popovertarget="quick-switch"
        aria-label="切换会话" :title="sessionName">
        <span class="min-w-0 truncate font-semibold">{{ sessionName }}</span><AppIcon name="down" class="shrink-0" />
      </button>
      <span class="terminal-status shrink-0 text-xs" :data-status="status" role="status">{{ statusText[status] }}</span>
      <button v-if="status === 'error'" class="control shrink-0 text-xs" @click="connect">重新连接</button>
      <button v-else-if="status === 'authExpired'" class="control shrink-0 text-xs" @click="reloadPage">刷新页面</button>
      <button class="control terminal-lock shrink-0" :aria-pressed="lockedWidth" aria-label="锁定原宽"
        :title="supportsLockWidth === false ? '旧版 worker 不支持锁定原宽' : '锁定原宽'"
        :disabled="status !== 'connected' || !supportsLockWidth" @click="toggleLockWidth">
        <AppIcon name="lock" />
      </button>
      <button class="control shrink-0" aria-label="终端工具" title="终端工具" popovertarget="terminal-tools">
        <AppIcon name="more" />
      </button>
    </header>

    <nav id="quick-switch" class="terminal-popover terminal-switcher" aria-label="快速切换会话" popover
      @toggle="refreshQuickSwitch">
      <p v-if="authExpired" role="status" class="text-sm text-[var(--limpet-danger)]">登录已失效</p>
      <template v-else>
        <p v-if="sessionsError" role="alert" class="text-sm text-[var(--limpet-danger)]">
          {{ sessionsError }} <button class="control" @click="emit('refreshSessions')">重试</button>
        </p>
        <p v-else-if="sessionsLoading && !sessions.length" class="text-sm text-[var(--limpet-muted)]">加载中…</p>
        <ul class="grid gap-1">
          <li v-for="session in sessions" :key="session.id" class="min-w-0">
            <button class="control w-full min-w-0 justify-start text-left" :aria-current="session.id === sessionId ? 'true' : undefined"
              :disabled="session.id === sessionId" @click="emit('openSession', session.id, session.name)">
              <span class="size-1.5 shrink-0 rounded-full"
                :class="session.attached ? 'bg-[var(--limpet-accent)]' : 'bg-[var(--limpet-muted)]'"></span>
              <span class="truncate">{{ session.name }}</span>
            </button>
          </li>
        </ul>
      </template>
      <button class="control mt-2 w-full justify-start" @click="emit('back')">
        <AppIcon name="back" /> 返回会话列表
      </button>
    </nav>

    <div id="terminal-tools" class="terminal-popover terminal-tools" aria-label="终端工具" popover>
      <div class="flex items-center gap-1 text-sm">
        <button class="control" aria-label="减小字号" title="减小字号" :disabled="fontSize <= 10" @click="setFontSize(fontSize - 1)">
          <AppIcon name="minus" />
        </button>
        <output aria-label="终端字号" class="min-w-6 text-center tabular-nums">{{ fontSize }}</output>
        <button class="control" aria-label="增大字号" title="增大字号" :disabled="fontSize >= 18" @click="setFontSize(fontSize + 1)">
          <AppIcon name="add" />
        </button>
        <button class="control" @click="setFontSize()">重置字号</button>
        <button class="control" aria-label="滚到底部" title="滚到底部" :disabled="!core" @click="scrollToBottom">
          <AppIcon name="bottom" />
        </button>
        <button class="control" aria-label="重载页面" title="重载页面" @click="reloadPage">
          <AppIcon name="refresh" />
        </button>
      </div>
      <p v-if="supportsLockWidth === false" role="status" class="mt-2 text-xs text-[var(--limpet-muted)]">
        旧版 worker 不支持锁定原宽
      </p>
    </div>

    <div v-if="fatalError" class="min-h-0 flex-1 overflow-auto p-4 text-sm text-[var(--limpet-danger)]">
      {{ fatalError }}
    </div>
    <div v-else class="terminal-viewport flex min-h-0 min-w-0 flex-1 flex-col"
      :class="{ 'terminal-locked': lockedWidth }" :style="terminalStyle"
      :inert="status === 'connecting' || status === 'reconnecting'">
      <div class="relative min-h-0 min-w-0 flex-1">
        <Terminal
          v-if="core"
          ref="term"
          :core="core"
          :renderer="terminalRenderer"
          :auto-resize="!lockedWidth"
          :cols="lockedWidth ? remoteSize?.cols : undefined"
          :rows="lockedWidth ? remoteSize?.rows : undefined"
          :scrollback-lines="10000"
          class="absolute inset-0 h-full w-full"
          @data="onData"
          @binary="onBinary"
          @resize="onResize"
          @ready="onReady"
          @error="onTerminalError"
        />
      </div>
      <div v-if="lockedWidth" v-show="railWidth" ref="rail" class="terminal-pan shrink-0"
        role="region" tabindex="0" aria-label="横向滚动终端" @scroll.passive="panTerminal">
        <div :style="{ width: railWidth + 'px' }"></div>
      </div>
    </div>

    <form v-if="composerOpen" class="terminal-footer flex shrink-0 items-end gap-1 border-t border-[var(--limpet-line)] bg-[var(--limpet-surface)] py-1"
      @submit.prevent="sendDraft(true)">
      <label for="terminal-draft" class="sr-only">终端输入</label>
      <textarea id="terminal-draft" ref="composerInput" v-model="draft" rows="2"
        class="terminal-draft min-w-0 flex-1 rounded-md border border-[var(--limpet-line)] bg-[var(--limpet-control)] px-2 py-1.5"
        autocapitalize="off" spellcheck="false" @keydown.enter="composerEnter"></textarea>
      <button type="button" class="control shrink-0 text-sm" :disabled="status !== 'connected' || !draft" @click="sendDraft(false)">仅发字</button>
      <button type="submit" class="control control-primary shrink-0 text-sm" :disabled="status !== 'connected'">发送</button>
    </form>

    <div class="terminal-footer terminal-keys min-w-0 shrink-0 items-stretch gap-1 border-t border-[var(--limpet-line)] bg-[var(--limpet-surface)] pt-1">
      <div class="flex min-w-0 flex-1 items-start gap-1 overflow-x-auto overscroll-x-contain" aria-label="终端快捷键">
        <button v-for="k in KEY_DEFS" :key="k.label" class="control shrink-0 text-sm" @click="sendKey(k.bytes)">{{ k.label }}</button>
        <button class="control shrink-0 text-sm" title="粘性 Ctrl：点亮后下一个字母以 Ctrl 组合发送"
          :aria-pressed="ctrlSticky" @click="toggleCtrl">Ctrl</button>
      </div>
      <div class="flex shrink-0 items-start border-l border-[var(--limpet-line)] pl-1">
        <button class="control" aria-label="输入条" title="输入条" :aria-expanded="composerOpen" @click="toggleComposer">
          <AppIcon name="composer" />
        </button>
      </div>
    </div>
  </main>
</template>
