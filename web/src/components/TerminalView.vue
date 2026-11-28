<script lang="ts">
// 模块级：WebGL2 探测结果按浏览器会话缓存一次。
let rendererProbe: 'dom' | 'webgl' | null = null

// 挂载 WebGL addon 前探测硬件加速：
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
// 终端视图：xterm.js ↔ WebSocket ↔ Go(worker PTY)。
// 帧约定：二进制帧 = 终端 I/O；文本帧 = 控制 JSON（后端→ready/size/ping，前端→resize/pong）。
// 接入由调用方给定的会话 ID 直接进行（会话已在列表中创建）。
// 断线自动重连（spec 005）：网络抖动、切网、锁屏恢复、server 重启之后自动接回原会话。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, shallowRef, useTemplateRef, watch } from 'vue'
import { FitAddon } from '@xterm/addon-fit'
import { Unicode11Addon } from '@xterm/addon-unicode11'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { WebglAddon } from '@xterm/addon-webgl'
import { Terminal, type ITheme } from '@xterm/xterm'
import '@xterm/xterm/css/xterm.css'
import { groupSessions, MAX_SCROLLBACK, type ScrollbackResult, type SessionItem } from '../sessions'
import { resolvedTheme, setThemeMode, themeMode, type ResolvedTheme, type ThemeMode } from '../theme'
import AppIcon from './AppIcon.vue'

const props = defineProps<{
  sessionId: string; sessionName: string; sessions: SessionItem[]
  sessionsLoading: boolean; sessionsError: string; authExpired: boolean
  scrollback: number
  loadScrollback: () => Promise<number>
  saveScrollback: (lines: number) => Promise<ScrollbackResult>
}>()
const emit = defineEmits<{
  back: []; openSession: [id: string, name: string]; refreshSessions: []; sessionAuthExpired: []
}>()
const workspaces = computed(() => groupSessions(props.sessions))

// server 每 20s 发一帧 ping；超过这个时限没收到任何消息即判定连接已死（与服务端的 60s 判定对称）。
const PING_TIMEOUT_MS = 60_000
const BACKOFF_START_MS = 1000
const BACKOFF_MAX_MS = 30_000
const WRITE_HIGH_WATERMARK = 16 * 1024 * 1024

const terminalFrame = useTemplateRef('terminalFrame')
const terminalHost = useTemplateRef('terminalHost')
const rail = useTemplateRef('rail')
const horizontalRange = ref(0)
const composerInput = useTemplateRef('composerInput')
const term = shallowRef<Terminal>()
const fatalError = ref('')
const scrollbackInput = useTemplateRef('scrollbackInput')
const scrollbackDraft = ref(String(props.scrollback))
const scrollbackDirty = ref(false)
const scrollbackSaving = ref(false)
const scrollbackInvalid = ref(false)
const scrollbackError = ref('')
const scrollbackFeedback = ref('')

// reconnecting 包含退避等待。ended / authExpired 是终态，不再重连。
type Status = 'connecting' | 'reconnecting' | 'connected' | 'ended' | 'authExpired' | 'error'
const status = ref<Status>('connecting')
type Dimensions = { cols: number; rows: number }
type Delivery = Uint8Array | Dimensions | 'fit'
type Connection = {
  generation: number
  socket: WebSocket
  queue: Delivery[]
  writingBytes: number
  scrollback: number
  draining: boolean
  resizing: boolean
  fit: FitAddon
  fitFrame: number
  stopLayout?: () => void
}
const encoder = new TextEncoder()
let connection: Connection | undefined
const lockPreference = ref(localStorage.getItem('limpet.lock-width') === 'true')
const supportsLockWidth = ref<boolean | null>(null)
const lockedWidth = ref(false)
const defaultFontSize = matchMedia('(min-width: 768px)').matches ? 13 : 11
const storedFontSize = Number(localStorage.getItem('limpet.font-size'))
const fontSize = ref(Number.isInteger(storedFontSize) && storedFontSize >= 10 && storedFontSize <= 18 ? storedFontSize : defaultFontSize)
const viewportHeight = ref<number>()
const viewportTop = ref(0)

// 终端配色：深色沿用原值；浅色按纸底（#f3f2ee）专门调校 ANSI 16 色，
// yellow/green/cyan/bright* 都用较深的饱和色，保证在浅灰底上清晰可读、不发荧光泛白。
const DARK_TERMINAL_THEME: ITheme = {
  background: '#121417', foreground: '#e6eaf0', cursor: '#34d399',
  selectionBackground: '#60a5fa66',
  black: '#242830', red: '#f87171', green: '#34d399', yellow: '#fbbf24',
  blue: '#60a5fa', magenta: '#c084fc', cyan: '#22d3ee', white: '#d8dde6',
  brightBlack: '#80899a', brightRed: '#fca5a5', brightGreen: '#6ee7b7', brightYellow: '#fde68a',
  brightBlue: '#93c5fd', brightMagenta: '#d8b4fe', brightCyan: '#67e8f9', brightWhite: '#f4f6fa',
}
const LIGHT_TERMINAL_THEME: ITheme = {
  background: '#f3f2ee', foreground: '#1f242c', cursor: '#0d9468', cursorAccent: '#f8faf9',
  selectionBackground: '#bfd8f2',
  black: '#333a44', red: '#c13033', green: '#116329', yellow: '#8a6500',
  blue: '#316dca', magenta: '#a03d9a', cyan: '#0b7a8b', white: '#8b929c',
  brightBlack: '#68707c', brightRed: '#d4443a', brightGreen: '#1a7f37', brightYellow: '#a97a0b',
  brightBlue: '#4383dd', brightMagenta: '#ad4bab', brightCyan: '#1188a0', brightWhite: '#565e6a',
}

function terminalTheme(theme: ResolvedTheme): ITheme {
  return theme === 'light' ? LIGHT_TERMINAL_THEME : DARK_TERMINAL_THEME
}

// 主题切换实时联动：只换配色，不断连 WebSocket、不重建终端实例。
watch(resolvedTheme, (theme) => {
  if (term.value) term.value.options.theme = terminalTheme(theme)
})

// 终端工具弹窗里的三态主题控件。
const THEME_MODES: { id: ThemeMode; label: string }[] = [
  { id: 'dark', label: '深色' },
  { id: 'light', label: '浅色' },
  { id: 'system', label: '跟随系统' },
]

// 重连状态。gen 是接入代数：旧连接的异步核对与交付回调不得触及新实例。
let gen = 0
let waitingToReconnect = false
let reconnectDelay = 0
let reconnectTimer = 0
let watchdog = 0
let lastMsgAt = 0
let unmounted = false

onMounted(() => {
  window.addEventListener('online', reconnectNow)
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
  document.removeEventListener('visibilitychange', reconnectNow)
  window.visualViewport?.removeEventListener('resize', syncViewport)
  window.visualViewport?.removeEventListener('scroll', syncViewport)
  clearTimeout(reconnectTimer)
  clearInterval(watchdog)
  disposeConnection()
})

function disposeConnection() {
  gen++
  const previous = connection
  connection = undefined
  if (previous) {
    previous.queue.length = 0
    cancelAnimationFrame(previous.fitFrame)
    previous.stopLayout?.()
    const { socket } = previous
    socket.onopen = socket.onmessage = socket.onclose = null
    socket.close()
  }
  term.value?.dispose()
  term.value = undefined
  horizontalRange.value = 0
  if (terminalHost.value) {
    terminalHost.value.replaceChildren()
    terminalHost.value.style.removeProperty('width')
  }
}

async function connect(confirmedScrollback?: number) {
  clearTimeout(reconnectTimer)
  reconnectTimer = 0
  waitingToReconnect = false
  disposeConnection()
  status.value = gen === 1 ? 'connecting' : 'reconnecting'
  fatalError.value = ''
  supportsLockWidth.value = null
  lockedWidth.value = false
  lastMsgAt = 0

  const attempt = gen
  const latestScrollback = await props.loadScrollback()
  if (unmounted || attempt !== gen) return
  if (props.authExpired) {
    status.value = 'authExpired'
    return
  }
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const socket = new WebSocket(`${proto}//${location.host}/api/ws/term/${encodeURIComponent(props.sessionId)}`)
  const current: Connection = {
    generation: gen, socket, queue: [], draining: false, resizing: false, fit: new FitAddon(), fitFrame: 0,
    writingBytes: 0, scrollback: confirmedScrollback ?? latestScrollback,
  }
  connection = current
  socket.binaryType = 'arraybuffer'
  socket.onopen = () => {
    if (connection !== current) return
    lastMsgAt = Date.now()
    if (lockPreference.value) socket.send('{"type":"resize","cols":0,"rows":0}')
  }
  socket.onmessage = (e) => {
    if (connection !== current) return
    lastMsgAt = Date.now()
    if (typeof e.data !== 'string') {
      deliver(current, new Uint8Array(e.data as ArrayBuffer))
      return
    }
    let ctl: { type: string } & Dimensions
    try {
      ctl = JSON.parse(e.data)
    } catch {
      return
    }
    if (ctl?.type === 'ready' && !term.value) {
      openTerminal(current, parseDimensions(ctl))
    } else if (ctl?.type === 'size' && lockedWidth.value) {
      const size = parseDimensions(ctl)
      if (size) deliver(current, size)
    } else if (ctl?.type === 'ping' && socket.readyState === WebSocket.OPEN) {
      socket.send('{"type":"pong"}')
    }
  }
  socket.onclose = (e) => {
    if (connection === current) void onDisconnected(e.code)
  }
}

// 断线分流：1000 是会话结束；其余（1006 网络断开或握手失败、1011 worker 连接异常、
// 4001 慢连接被丢弃、4002 心跳超时）先核对会话是否还在，再决定重连还是进入终态。
async function onDisconnected(code: number) {
  if (unmounted) return
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
  if (!connection || status.value !== 'connected') return
  if (Date.now() - lastMsgAt <= PING_TIMEOUT_MS) return
  scheduleReconnect()
}

function parseDimensions(ctl: Dimensions): Dimensions | undefined {
  const { cols, rows } = ctl
  if (Number.isInteger(cols) && cols >= 2 && cols <= 500 && Number.isInteger(rows) && rows >= 2 && rows <= 200) {
    return { cols, rows }
  }
}

function deliver(current: Connection, event: Delivery) {
  current.queue.push(event)
  drain(current)
}

function drain(current: Connection) {
  const terminal = term.value
  if (!terminal || connection !== current || current.draining || current.resizing) return

  current.draining = true
  let consumed = 0
  try {
    while (consumed < current.queue.length && !current.resizing && current.writingBytes < WRITE_HIGH_WATERMARK) {
      const event = current.queue[consumed++]!
      if (event instanceof Uint8Array) {
        const bytes = event.byteLength
        current.writingBytes += bytes
        terminal.write(event, () => {
          if (current.generation !== gen || term.value !== terminal) return
          current.writingBytes -= bytes
          drain(current)
        })
        continue
      }

      // write 的回调可能同步执行。先占住屏障，后续帧始终留在 FIFO 中。
      current.resizing = true
      terminal.write('', () => {
        if (current.generation !== gen || term.value !== terminal) return
        try {
          if (event === 'fit') fitTerminal(terminal, current.fit)
          else terminal.resize(event.cols, event.rows)
        } catch (error) {
          failTerminal(error)
          return
        }
        current.resizing = false
        drain(current)
      })
    }
  } catch (error) {
    failTerminal(error)
  } finally {
    current.queue.splice(0, consumed)
    current.draining = false
  }
}

function fitTerminal(terminal: Terminal, fit: FitAddon) {
  const size = fit.proposeDimensions()
  if (!size || !Number.isFinite(size.cols) || !Number.isFinite(size.rows)) return
  // FitAddon 允许一行，超大浏览器也能测出超过 worker 协议上限的尺寸。
  if (parseDimensions(size)) fit.fit()
  else terminal.resize(Math.min(500, size.cols), Math.min(200, Math.max(2, size.rows)))
}

function reportResize(current: Connection, { cols, rows }: Dimensions) {
  if (connection === current && !lockedWidth.value && current.socket.readyState === WebSocket.OPEN) {
    current.socket.send(JSON.stringify({ type: 'resize', cols, rows }))
  }
}

function openTerminal(current: Connection, remoteSize?: Dimensions) {
  const host = terminalHost.value!
  reconnectDelay = 0
  supportsLockWidth.value = remoteSize !== undefined
  lockedWidth.value = lockPreference.value && remoteSize !== undefined
  const terminal = new Terminal({
    ...(lockedWidth.value ? remoteSize : undefined),
    allowProposedApi: true,
    scrollback: current.scrollback === 0 ? MAX_SCROLLBACK : current.scrollback,
    fontFamily: getComputedStyle(host).getPropertyValue('--limpet-mono').trim(),
    fontSize: fontSize.value,
    theme: terminalTheme(resolvedTheme.value),
  })
  term.value = terminal
  try {
    terminal.loadAddon(current.fit)
    terminal.loadAddon(new Unicode11Addon())
    terminal.unicode.activeVersion = '11'
    terminal.loadAddon(new WebLinksAddon())
    terminal.open(host)
    if (detectRenderer() === 'webgl') {
      let addon: WebglAddon | undefined
      try {
        const webgl = addon = new WebglAddon()
        terminal.loadAddon(webgl)
        webgl.onContextLoss(() => webgl.dispose())
      } catch {
        addon?.dispose()
      }
    }
    terminal.onData((chunk) => {
      if (connection === current) onData(chunk)
    })
    terminal.onBinary((chunk) => {
      if (connection !== current) return
      const bytes = new Uint8Array(chunk.length)
      for (let i = 0; i < chunk.length; i++) bytes[i] = chunk.charCodeAt(i) & 0xff
      sendInput(bytes)
    })
    terminal.attachCustomKeyEventHandler((event) => {
      if (event.type !== 'keydown' || event.altKey || event.isComposing || event.ctrlKey === event.metaKey) return true
      const key = event.key.toLowerCase()
      // 拦截 xterm 的按键编码，保留浏览器 paste 事件的默认动作。
      if (key === 'v') return false
      if (key !== 'c' || !terminal.hasSelection()) return true
      const text = terminal.getSelection()
      if (navigator.clipboard) {
        event.preventDefault()
        void navigator.clipboard.writeText(text).catch(() => {
          if (term.value === terminal && terminal.getSelection() === text) document.execCommand('copy')
        })
      }
      // Clipboard API 不可用时，让浏览器的 copy 事件交给 xterm 处理。
      return false
    })
    if (!lockedWidth.value) {
      fitTerminal(terminal, current.fit)
      reportResize(current, terminal)
      terminal.onResize((size) => reportResize(current, size))
    }
    observeLayout(current, terminal, host)
    status.value = 'connected'
    drain(current)
  } catch (error) {
    failTerminal(error)
    return
  }
  if (connection !== current) return
  emit('refreshSessions')
  void nextTick(() => {
    const focus = document.activeElement
    if (connection === current && status.value === 'connected' &&
        focus !== composerInput.value && !focus?.closest('dialog[open], .terminal-popover, button[popovertarget]') &&
        !document.querySelector('.terminal-popover:popover-open')) {
      terminal.focus()
    }
  })
}

async function refreshTerminalTools(event: ToggleEvent) {
  if (event.newState !== 'open') return
  if (!scrollbackDirty.value) scrollbackDraft.value = String(props.scrollback)
  const lines = await props.loadScrollback()
  if (!scrollbackDirty.value) scrollbackDraft.value = String(lines)
}

function editScrollback() {
  const input = scrollbackInput.value!
  scrollbackDraft.value = input.value
  scrollbackDirty.value = true
  scrollbackInvalid.value = false
  scrollbackError.value = ''
  scrollbackFeedback.value = ''
}

async function submitScrollback() {
  if (scrollbackSaving.value) return
  const input = scrollbackInput.value!
  const draft = input.value
  const lines = input.valueAsNumber
  scrollbackFeedback.value = ''
  if (draft === '' || input.validity.badInput || !Number.isInteger(lines) || lines < 0 || lines > MAX_SCROLLBACK) {
    scrollbackInvalid.value = true
    scrollbackError.value = '请输入 0 到 1000000 之间的整数'
    input.focus()
    return
  }
  scrollbackError.value = ''
  scrollbackInvalid.value = false
  scrollbackSaving.value = true
  try {
    const result = await props.saveScrollback(lines)
    if (unmounted) return
    if (scrollbackDraft.value === draft) {
      scrollbackDraft.value = String(result.scrollback)
      scrollbackDirty.value = false
    }
    const details: string[] = []
    if (result.unsupported > 0) details.push(`${result.unsupported} 个旧版会话需新建后生效`)
    if (result.failed > 0) details.push(`${result.failed} 个会话未同步`)
    scrollbackFeedback.value = details.length ? `已保存（${details.join('、')}）` : '已保存'
    if (connection && connection.scrollback !== result.scrollback && status.value !== 'ended') {
      void connect(result.scrollback)
    }
  } catch (error) {
    scrollbackError.value = error instanceof Error ? error.message : String(error)
  } finally {
    scrollbackSaving.value = false
  }
}

// 输入闸门：WS 打开 + 已收到 ready。接入中与重连中的按键直接丢弃，不缓冲。
function sendInput(bytes: Uint8Array): boolean {
  const sock = connection?.socket
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
  if (ctrlSticky.value && chunk.length === 1) {
    let code = chunk.charCodeAt(0)
    if (code >= 97 && code <= 122) code -= 32
    if (code >= 64 && code <= 95) {
      if (sendInput(Uint8Array.of(code & 0x1f))) ctrlSticky.value = false
      return
    }
  }
  // 自动查询回包是多字节序列，直接发送，不消耗下一个字母的 Ctrl。
  sendInput(encoder.encode(chunk))
}

function failTerminal(error: unknown) {
  waitingToReconnect = false
  clearTimeout(reconnectTimer)
  reconnectTimer = 0
  disposeConnection()
  fatalError.value = error instanceof Error ? error.message : String(error)
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
  if (connection) scheduleFit(connection)
}

function scheduleFit(current: Connection) {
  if (lockedWidth.value || current.fitFrame || !term.value) return
  current.fitFrame = requestAnimationFrame(() => {
    current.fitFrame = 0
    if (connection === current) deliver(current, 'fit')
  })
}

function observeLayout(current: Connection, terminal: Terminal, host: HTMLElement) {
  const frame = terminalFrame.value!
  const locked = lockedWidth.value
  const screen = host.querySelector<HTMLElement>('.xterm-screen')!
  const listeners = new AbortController()
  let following = true
  let lastHeight = -1
  let lastWidth = -1
  const verticalBar = host.querySelector<HTMLElement>('.xterm-scrollable-element > .scrollbar.vertical')!

  const observer = new ResizeObserver(() => {
    if (connection !== current) return
    if (!locked) {
      scheduleFit(current)
      return
    }

    const width = screen.getBoundingClientRect().width + verticalBar.getBoundingClientRect().width
    if (width !== lastWidth) {
      lastWidth = width
      host.style.width = width + 'px'
    }
    const clientHeight = frame.clientHeight
    if (clientHeight !== lastHeight || following) {
      lastHeight = clientHeight
      if (following) frame.scrollTop = frame.scrollHeight
    }
    const overflow = frame.scrollWidth - frame.clientWidth
    const range = overflow > 1 ? overflow : 0
    if (horizontalRange.value !== range) {
      horizontalRange.value = range
      void nextTick(() => {
        if (connection === current) syncRail()
      })
    }
  })
  observer.observe(frame)
  if (locked) {
    observer.observe(screen)
    frame.addEventListener('scroll', () => {
      if (frame.clientHeight === lastHeight) {
        following = frame.scrollHeight - frame.clientHeight - frame.scrollTop <= 1
      }
      syncRail()
    }, { passive: true, signal: listeners.signal })
  }

  let touch: { x: number; y: number; remaining: number; history?: boolean } | undefined
  host.addEventListener('touchstart', (event) => {
    const point = event.touches.length === 1 ? event.touches[0] : undefined
    touch = point ? { x: point.clientX, y: point.clientY, remaining: 0 } : undefined
  }, { passive: true, signal: listeners.signal })
  host.addEventListener('touchmove', (event) => {
    const point = event.touches.length === 1 ? event.touches[0] : undefined
    if (!touch || !point || !event.cancelable || terminal.modes.mouseTrackingMode !== 'none') return
    const dy = touch.y - point.clientY
    if (touch.history === undefined) {
      const dx = Math.abs(point.clientX - touch.x)
      if (dy === 0 && dx === 0) return
      // 超高网格先让浏览器沿该方向滚动外层，到边后再由 xterm 滚动历史。
      const outerCanScroll = locked && (dy > 0
        ? frame.scrollTop + frame.clientHeight < frame.scrollHeight - 1
        : frame.scrollTop > 1)
      const buffer = terminal.buffer.active
      const historyCanScroll = dy > 0 ? buffer.viewportY < buffer.baseY : buffer.viewportY > 0
      touch.history = Math.abs(dy) > dx && !outerCanScroll && historyCanScroll
    }
    if (!touch.history) return

    event.preventDefault()
    touch.y = point.clientY
    touch.remaining += dy
    const cellHeight = screen.getBoundingClientRect().height / terminal.rows
    const lines = Math.trunc(touch.remaining / cellHeight)
    if (lines) {
      terminal.scrollLines(lines)
      touch.remaining -= lines * cellHeight
    }
  }, { passive: false, signal: listeners.signal })

  current.stopLayout = () => {
    observer.disconnect()
    listeners.abort()
  }
}

function syncRail() {
  const frame = terminalFrame.value
  const bar = rail.value
  if (frame && bar && bar.scrollLeft !== frame.scrollLeft) bar.scrollLeft = frame.scrollLeft
}

function panTerminal() {
  const frame = terminalFrame.value
  const bar = rail.value
  if (frame && bar && frame.scrollLeft !== bar.scrollLeft) frame.scrollLeft = bar.scrollLeft
}

function scrollToBottom() {
  term.value?.scrollToBottom()
  if (lockedWidth.value && terminalFrame.value) {
    terminalFrame.value.scrollTop = terminalFrame.value.scrollHeight
  }
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
  if (term.value && connection) {
    term.value.options.fontSize = fontSize.value
    scheduleFit(connection)
  }
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
      <button class="control min-w-0 me-auto justify-start bg-transparent text-left hover:bg-[var(--limpet-pressed)] active:bg-[var(--limpet-pressed)]" popovertarget="quick-switch"
        aria-label="切换会话" :title="sessionName">
        <span class="min-w-0 truncate font-semibold">{{ sessionName }}</span><AppIcon name="down" class="shrink-0" />
      </button>
      <span class="terminal-status shrink-0 text-xs" :data-status="status" role="status">{{ statusText[status] }}</span>
      <button v-if="status === 'error'" class="control shrink-0 text-xs" @click="connect()">重新连接</button>
      <button v-else-if="status === 'authExpired'" class="control shrink-0 text-xs" @click="reloadPage">刷新页面</button>
      <button class="control shrink-0" aria-label="滚到底部" title="滚到底部" :disabled="!term" @click="scrollToBottom">
        <AppIcon name="bottom" />
      </button>
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
        <section v-for="group in workspaces" :key="group.key" class="mb-2 min-w-0">
          <h2 class="truncate px-2 py-1 text-xs text-[var(--limpet-muted)]">{{ group.title }}</h2>
          <ul class="grid gap-1">
            <li v-for="session in group.items" :key="session.id" class="min-w-0">
              <button class="control w-full min-w-0 justify-start text-left" :aria-current="session.id === sessionId ? 'true' : undefined"
                :disabled="session.id === sessionId" @click="emit('openSession', session.id, session.name)">
                <span class="size-1.5 shrink-0 rounded-full"
                  :class="session.attached ? 'bg-[var(--limpet-accent)]' : 'bg-[var(--limpet-muted)]'"></span>
                <span class="truncate">{{ session.name }}</span>
              </button>
            </li>
          </ul>
        </section>
      </template>
      <button class="control mt-2 w-full justify-start" @click="emit('back')">
        <AppIcon name="back" /> 返回会话列表
      </button>
    </nav>

    <div id="terminal-tools" class="terminal-popover terminal-tools" aria-label="终端工具" popover
      @toggle="refreshTerminalTools">
      <div class="flex items-center gap-1 text-sm">
        <button class="control" aria-label="减小字号" title="减小字号" :disabled="fontSize <= 10" @click="setFontSize(fontSize - 1)">
          <AppIcon name="minus" />
        </button>
        <output aria-label="终端字号" class="min-w-6 text-center tabular-nums">{{ fontSize }}</output>
        <button class="control" aria-label="增大字号" title="增大字号" :disabled="fontSize >= 18" @click="setFontSize(fontSize + 1)">
          <AppIcon name="add" />
        </button>
        <button class="control" @click="setFontSize()">重置字号</button>
        <button class="control" aria-label="重载页面" title="重载页面" @click="reloadPage">
          <AppIcon name="refresh" />
        </button>
      </div>
      <div class="mt-2 flex min-w-0 items-center gap-1 border-t border-[var(--limpet-line)] pt-2" role="group"
        aria-label="界面主题">
        <span class="shrink-0 text-sm text-[var(--limpet-muted)]">主题</span>
        <div class="ml-auto flex min-w-0 gap-1">
          <button v-for="m in THEME_MODES" :key="m.id" class="control text-xs"
            :class="themeMode === m.id ? 'text-[var(--limpet-accent)]' : ''"
            :aria-pressed="themeMode === m.id" @click="setThemeMode(m.id)">{{ m.label }}</button>
        </div>
      </div>
      <form class="terminal-scrollback mt-2 grid gap-1 border-t border-[var(--limpet-line)] pt-2" novalidate
        @submit.prevent="submitScrollback">
        <label for="terminal-scrollback" class="text-sm">回滚行数</label>
        <div class="flex min-w-0 gap-2">
          <input id="terminal-scrollback" ref="scrollbackInput" type="number" inputmode="numeric" min="0"
            :max="MAX_SCROLLBACK" step="1" required
            class="min-w-0 flex-1 rounded-md border border-[var(--limpet-line)] bg-[var(--limpet-canvas)] px-2 text-base tabular-nums"
            :value="scrollbackDraft" :aria-invalid="scrollbackInvalid"
            :aria-describedby="scrollbackInvalid ? 'scrollback-hint scrollback-error' : 'scrollback-hint'"
            @input="editScrollback" />
          <button class="control shrink-0" type="submit" :disabled="scrollbackSaving || authExpired">应用</button>
        </div>
        <p id="scrollback-hint" class="min-w-0 text-xs wrap-anywhere"
          :class="scrollback === 0 || (scrollbackDraft !== '' && Number(scrollbackDraft) === 0)
            ? 'text-[var(--limpet-warn)]' : 'text-[var(--limpet-muted)]'">
          0 表示近似无限（浏览器上限 100 万行）；设为无限且历史过多时，可能导致内存升高、重连变慢或页面卡顿
        </p>
        <p v-if="scrollbackError" id="scrollback-error" role="alert" class="text-xs wrap-anywhere text-[var(--limpet-danger)]">
          {{ scrollbackError }}
        </p>
        <p v-if="scrollbackFeedback" role="status" class="text-xs wrap-anywhere text-[var(--limpet-muted)]">
          {{ scrollbackFeedback }}
        </p>
      </form>
      <p v-if="supportsLockWidth === false" role="status" class="mt-2 text-xs text-[var(--limpet-muted)]">
        旧版 worker 不支持锁定原宽
      </p>
    </div>

    <div v-if="fatalError" class="min-h-0 flex-1 overflow-auto p-4 text-sm text-[var(--limpet-danger)]">
      {{ fatalError }}
    </div>
    <div v-else class="terminal-viewport flex min-h-0 min-w-0 flex-1 flex-col"
      :class="{ 'terminal-locked': lockedWidth }"
      :inert="status === 'connecting' || status === 'reconnecting'">
      <div ref="terminalFrame" class="terminal-frame min-h-0 min-w-0 flex-1">
        <div ref="terminalHost" class="terminal-host"></div>
      </div>
      <div v-show="lockedWidth && horizontalRange" ref="rail" class="terminal-pan shrink-0"
        role="region" tabindex="0" aria-label="横向滚动终端" @scroll.passive="panTerminal">
        <div :style="{ width: `calc(100% + ${horizontalRange}px)` }"></div>
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
