<script setup lang="ts">
// 终端视图：wterm(ghostty core) ↔ WebSocket ↔ Go(worker PTY)。
// 帧约定：二进制帧 = 终端 I/O；文本帧 = 控制 JSON（后端→ready/ping，前端→resize/pong）。
// 接入由调用方给定的会话 ID 直接进行（会话已在列表中创建）。
// 断线自动重连（spec 005）：网络抖动、切网、锁屏恢复、server 重启之后自动接回原会话。
import { nextTick, onBeforeUnmount, onMounted, ref, useTemplateRef } from 'vue'
import { Terminal, type WTerm } from '@wterm/vue'
import { GhosttyCore } from '@wterm/ghostty'
import '@wterm/vue/css'

const props = defineProps<{ sessionId: string; sessionName: string }>()
const emit = defineEmits<{ back: [] }>()

// server 每 20s 发一帧 ping；超过这个时限没收到任何消息即判定连接已死（与服务端的 60s 判定对称）。
const PING_TIMEOUT_MS = 60_000
const BACKOFF_START_MS = 1000
const BACKOFF_MAX_MS = 30_000

const term = useTemplateRef('term')
const core = ref<GhosttyCore | null>(null)
// fatalError：core 加载或 Terminal 初始化失败，终端组件无法存在，
// 以全屏错误替代终端；重新连接会重建 core 并重新挂载组件。
const fatalError = ref('')

// reconnecting：自动重连进行中（含退避等待）。ended / authExpired 是终态，不再重连。
type Status = 'connecting' | 'reconnecting' | 'connected' | 'ended' | 'authExpired' | 'error'
const status = ref<Status>('connecting')

const encoder = new TextEncoder()
let ws: WebSocket | null = null
// Terminal 组件挂载后长期存活，@ready 在其生命周期内只触发一次；
// ready 只置位一次、绝不随 connect() 重置，否则重连后输出会永远排队。
let termReady = false
// ready 之前到达的输出先排队（重建帧 + 实时流都在 ready 之后）。
const pending: Uint8Array[] = []
// autoResize 的首次回调早于 WS 握手完成，必须把尺寸存下来，
// 在 onopen 时补发，否则 worker 永远停在初始 80x24。
let lastSize: { cols: number; rows: number } | null = null

// 重连状态。gen 是接入代数：connect() 中途有 await，期间若发起了新的接入，旧的那次自行作废。
let gen = 0
let attempts = 0
let reconnectDelay = BACKOFF_START_MS
let reconnectTimer = 0
let watchdog = 0
let lastMsgAt = 0
let unmounted = false

onMounted(() => {
  window.addEventListener('online', reconnectNow)
  document.addEventListener('visibilitychange', reconnectNow)
  watchdog = window.setInterval(checkLiveness, 1000)
  void connect()
})

onBeforeUnmount(() => {
  unmounted = true
  window.removeEventListener('online', reconnectNow)
  document.removeEventListener('visibilitychange', reconnectNow)
  clearTimeout(reconnectTimer)
  clearInterval(watchdog)
  detachSocket()
})

function detachSocket() {
  if (!ws) return
  // 先摘回调再关闭，避免旧连接的 onclose 污染新一轮连接的状态。
  ws.onopen = ws.onmessage = ws.onclose = null
  ws.close()
  ws = null
}

async function connect() {
  const my = ++gen
  clearTimeout(reconnectTimer)
  reconnectTimer = 0
  detachSocket()
  pending.length = 0
  status.value = attempts === 0 ? 'connecting' : 'reconnecting'
  attempts++
  // 每次接入都重建 core 并重新挂载终端组件，让本次接入拥有干净的
  // 屏幕状态。组件重挂载会重新触发 @ready，termReady 随之复位。
  core.value = null
  termReady = false
  fatalError.value = ''
  lastSize = null as { cols: number; rows: number } | null

  try {
    core.value = await GhosttyCore.load()
  } catch (e) {
    if (my !== gen) return
    fatalError.value = e instanceof Error ? e.message : String(e)
    status.value = 'error'
    return
  }
  if (my !== gen) return

  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const sock = new WebSocket(`${proto}//${location.host}/api/ws/term/${encodeURIComponent(props.sessionId)}`)
  ws = sock
  sock.binaryType = 'arraybuffer'
  // 收到 ready 帧才算「已连接」并开放输入。
  sock.onopen = () => {
    if (ws !== sock) return
    lastMsgAt = Date.now()
    sendResize()
  }
  sock.onmessage = (e) => {
    if (ws !== sock) return
    lastMsgAt = Date.now()
    if (typeof e.data === 'string') {
      // 文本帧 = 控制 JSON。
      try {
        const ctl = JSON.parse(e.data)
        if (ctl.type === 'ready') {
          reconnectDelay = BACKOFF_START_MS
          status.value = 'connected'
          // 等 Vue 摘掉 inert 再聚焦，否则 focus 会被吞掉。
          nextTick(() => term.value?.focus())
        } else if (ctl.type === 'ping' && sock.readyState === WebSocket.OPEN) {
          sock.send('{"type":"pong"}')
        }
      } catch { /* 忽略无法解析的控制帧 */ }
      return
    }
    const bytes = new Uint8Array(e.data as ArrayBuffer)
    if (!termReady) {
      pending.push(bytes)
    } else {
      term.value?.write(bytes)
    }
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
  if (code === 1000) {
    status.value = 'ended'
    return
  }
  const verdict = await checkSession()
  if (unmounted) return
  if (verdict === 'gone') {
    status.value = 'ended'
  } else if (verdict === 'auth') {
    status.value = 'authExpired'
  } else {
    scheduleReconnect()
  }
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
  status.value = 'reconnecting'
  const delay = reconnectDelay
  reconnectDelay = Math.min(reconnectDelay * 2, BACKOFF_MAX_MS)
  reconnectTimer = window.setTimeout(() => {
    reconnectTimer = 0
    void connect()
  }, delay)
}

// 网络恢复或页面回到前台：正在等退避就立即试一次。
function reconnectNow() {
  if (unmounted || document.visibilityState === 'hidden') return
  if (status.value === 'reconnecting' && reconnectTimer !== 0) void connect()
}

// 连接无声死亡时浏览器不会触发 close（手机切网的常态）。server 每 20s 发 ping，
// 60s 没收到任何消息就当作已死：主动关闭并重连。
function checkLiveness() {
  if (!ws || status.value !== 'connected') return
  if (Date.now() - lastMsgAt <= PING_TIMEOUT_MS) return
  void connect()
}

function flushPending() {
  for (const bytes of pending) term.value?.write(bytes)
  pending.length = 0
}

// 输入闸门：WS 打开 + 已收到 ready。接入中与重连中的按键直接丢弃，不缓冲。
function sendInput(bytes: Uint8Array) {
  if (ws?.readyState !== WebSocket.OPEN || status.value !== 'connected') return
  const CHUNK = 64 * 1024
  for (let i = 0; i < bytes.length; i += CHUNK) {
    ws.send(bytes.slice(i, i + CHUNK))
  }
}

function onData(chunk: string) {
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
  sendInput(new Uint8Array(bytes))
}

function sendResize() {
  if (ws?.readyState === WebSocket.OPEN && lastSize) {
    ws.send(JSON.stringify({ type: 'resize', ...lastSize }))
  }
}

function onResize(cols: number, rows: number) {
  lastSize = { cols, rows }
  sendResize()
}

function onReady(wt: WTerm) {
  // 组件生命周期内只触发一次；不重挂载就不会再来。
  // 初始网格与默认尺寸（80x24）一致时 ResizeObserver 不触发 @resize，
  // 首个尺寸必须直接从实例读取，否则后端会等满 3s 超时兜底。
  lastSize = { cols: wt.cols, rows: wt.rows }
  sendResize()
  termReady = true
  flushPending()
}

// WTerm 初始化失败（wasm 加载/实例化问题）：终端不可用，转为 fatalError；
// 下次重连会重建 core 并重新挂载组件。
function onTerminalError(e: unknown) {
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
  sendInput(encoder.encode(bytes))
  term.value?.focus()
}

function toggleCtrl() {
  ctrlSticky.value = !ctrlSticky.value
  term.value?.focus()
}

function goBack() {
  emit('back')
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
  <div
    class="flex h-dvh flex-col bg-neutral-950 text-neutral-200 pt-[env(safe-area-inset-top)]"
  >
    <header class="flex items-center gap-2 px-3 py-2 text-sm">
      <button
        class="h-9 w-9 shrink-0 rounded-md text-lg text-neutral-400 active:bg-neutral-800 hover:text-neutral-200"
        title="返回列表"
        @click="goBack"
      >
        ←
      </button>
      <span class="truncate font-semibold tracking-tight">{{ sessionName }}</span>
      <span
        class="shrink-0 rounded-full px-2 py-0.5 text-xs"
        :class="{
          'bg-neutral-800 text-neutral-400': status === 'connecting' || status === 'ended',
          'bg-amber-900/60 text-amber-400': status === 'reconnecting',
          'bg-emerald-900/60 text-emerald-400': status === 'connected',
          'bg-red-900/60 text-red-400': status === 'authExpired' || status === 'error',
        }"
      >
        {{ statusText[status] }}
      </span>
      <button
        v-if="status === 'error'"
        class="ml-auto shrink-0 rounded-md bg-neutral-800 px-3 py-1.5 text-xs active:scale-95 hover:bg-neutral-700"
        @click="connect"
      >
        重新连接
      </button>
      <button
        v-else-if="status === 'authExpired'"
        class="ml-auto shrink-0 rounded-md bg-neutral-800 px-3 py-1.5 text-xs active:scale-95 hover:bg-neutral-700"
        @click="reloadPage"
      >
        刷新页面
      </button>
    </header>

    <div v-if="fatalError" class="flex-1 overflow-auto p-6 font-mono text-sm text-red-400">
      {{ fatalError }}
    </div>

    <div
      v-else
      class="min-h-0 flex-1 px-2"
      :inert="status === 'connecting' || status === 'reconnecting'"
    >
      <Terminal
        v-if="core"
        ref="term"
        :core="core"
        auto-resize
        :scrollback-lines="10000"
        class="h-full w-full"
        @data="onData"
        @binary="onBinary"
        @resize="onResize"
        @ready="onReady"
        @error="onTerminalError"
      />
    </div>

    <!-- 移动按键栏（charter 验收 3）：桌面视口隐藏 -->
    <div
      class="flex gap-1 overflow-x-auto border-t border-neutral-800 bg-neutral-900/80 px-1 py-1 pb-[max(env(safe-area-inset-bottom),0.25rem)] touch-manipulation md:hidden"
    >
      <button
        v-for="k in KEY_DEFS"
        :key="k.label"
        class="min-w-[3.25rem] shrink-0 rounded-md px-2 py-2 text-sm active:bg-neutral-600"
        :class="k.label === 'Ctrl+C' && ctrlSticky ? 'bg-emerald-700 text-white' : 'bg-neutral-800 text-neutral-200'"
        @click="sendKey(k.bytes)"
      >
        {{ k.label }}
      </button>
      <button
        class="min-w-[3.25rem] shrink-0 rounded-md px-2 py-2 text-sm active:bg-neutral-600"
        :class="ctrlSticky ? 'bg-emerald-700 text-white' : 'bg-neutral-800 text-neutral-200'"
        title="粘性 Ctrl：点亮后下一个字母以 Ctrl 组合发送"
        @click="toggleCtrl"
      >
        Ctrl
      </button>
    </div>
  </div>
</template>
