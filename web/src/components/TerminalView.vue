<script setup lang="ts">
// 终端视图：wterm(ghostty core) ↔ WebSocket ↔ Go(tmux 客户端 PTY)。
// 帧约定：二进制帧 = 终端 I/O；文本帧 = 控制 JSON（后端→ready）。
// 接入由调用方给定的会话 ID 直接进行（会话已在列表中创建）。
import { nextTick, onBeforeUnmount, onMounted, ref, useTemplateRef } from 'vue'
import { Terminal, type WTerm } from '@wterm/vue'
import { GhosttyCore } from '@wterm/ghostty'
import '@wterm/vue/css'

const props = defineProps<{ sessionId: string }>()
const emit = defineEmits<{ back: [] }>()

const term = useTemplateRef('term')
const core = ref<GhosttyCore | null>(null)
// 两类错误分开处理：
// - fatalError：core 加载或 Terminal 初始化失败，终端组件无法存在，
//   以全屏错误替代终端；重连会重建 core 并重新挂载组件。
// - connError：接入失败，终端组件完好，错误显示在头部。
const fatalError = ref('')
const connError = ref('')

type Status = 'connecting' | 'connected' | 'closed' | 'error'
const status = ref<Status>('connecting')

const encoder = new TextEncoder()
let ws: WebSocket | null = null
// Terminal 组件挂载后长期存活，@ready 在其生命周期内只触发一次；
// ready 只置位一次、绝不随 connect() 重置，否则重连后输出会永远排队。
let termReady = false
// ready 之前到达的输出先排队，避免丢首屏（tmux 重绘）。
const pending: Uint8Array[] = []
// autoResize 的首次回调早于 WS 握手完成，必须把尺寸存下来，
// 在 onopen 时补发，否则 tmux 客户端永远停在初始 80x24。
let lastSize: { cols: number; rows: number } | null = null

onMounted(connect)

onBeforeUnmount(() => detachSocket())

function detachSocket() {
  if (!ws) return
  // 先摘回调再关闭，避免旧连接的 onclose 污染新一轮连接的状态。
  ws.onopen = ws.onmessage = ws.onclose = null
  ws.close()
  ws = null
}

async function connect() {
  detachSocket()
  pending.length = 0
  status.value = 'connecting'
  connError.value = ''
  // 每次接入都重建 core 并重新挂载终端组件，让本次接入拥有干净的
  // 屏幕状态。组件重挂载会重新触发 @ready，termReady 随之复位。
  core.value = null
  termReady = false
  fatalError.value = ''
  lastSize = null as { cols: number; rows: number } | null

  try {
    core.value = await GhosttyCore.load()
  } catch (e) {
    fatalError.value = e instanceof Error ? e.message : String(e)
    status.value = 'error'
    return
  }

  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  ws = new WebSocket(`${proto}//${location.host}/api/ws/term/${encodeURIComponent(props.sessionId)}`)
  ws.binaryType = 'arraybuffer'
  // onopen 不等于可用：后端要等首条尺寸消息才启动 tmux 客户端。
  // 发送当前尺寸触发启动，收到 ready 帧后才显示「已连接」并开放输入。
  ws.onopen = () => {
    sendResize()
  }
  ws.onmessage = (e) => {
    if (typeof e.data === 'string') {
      // 文本帧 = 控制 JSON（目前只有后端→ready）。
      try {
        const ctl = JSON.parse(e.data)
        if (ctl.type === 'ready') {
          status.value = 'connected'
          // 等 Vue 移除 inert 后再聚焦，否则 focus 会被吞掉。
          nextTick(() => term.value?.focus())
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
  ws.onclose = (e) => {
    if (e.code === 1011) {
      // 后端以 1011 关闭 = 客户端启动失败，与普通断开区分开。
      connError.value = '终端启动失败'
      status.value = 'error'
      return
    }
    if (status.value !== 'error') status.value = 'closed'
  }
}

function flushPending() {
  for (const bytes of pending) term.value?.write(bytes)
  pending.length = 0
}

// 输入闸门：WS 打开 + 已收到 ready。接入中不发输入帧，
// 后端对启动前的二进制输入也会以 1008 显式拒绝。
function sendInput(bytes: Uint8Array) {
  if (ws?.readyState !== WebSocket.OPEN || status.value !== 'connected') return
  const CHUNK = 64 * 1024
  for (let i = 0; i < bytes.length; i += CHUNK) {
    ws.send(bytes.slice(i, i + CHUNK))
  }
}

function onData(chunk: string) {
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
// pointerdown + preventDefault 避免按钮抢走终端焦点。
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

function sendKey(bytes: string, e: PointerEvent) {
  e.preventDefault()
  sendInput(encoder.encode(bytes))
}

function goBack() {
  emit('back')
}

const statusText = {
  connecting: '接入中',
  connected: '已连接',
  closed: '已断开',
  error: '错误',
} as const
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
      <span class="truncate font-semibold tracking-tight">{{ sessionId }}</span>
      <span
        class="shrink-0 rounded-full px-2 py-0.5 text-xs"
        :class="{
          'bg-neutral-800 text-neutral-400': status === 'connecting',
          'bg-emerald-900/60 text-emerald-400': status === 'connected',
          'bg-amber-900/60 text-amber-400': status === 'closed',
          'bg-red-900/60 text-red-400': status === 'error',
        }"
      >
        {{ statusText[status] }}
      </span>
      <span v-if="connError" class="truncate text-xs text-red-400">{{ connError }}</span>
      <button
        v-if="status === 'closed' || status === 'error'"
        class="ml-auto shrink-0 rounded-md bg-neutral-800 px-3 py-1.5 text-xs active:scale-95 hover:bg-neutral-700"
        @click="connect"
      >
        重新连接
      </button>
    </header>

    <div v-if="fatalError" class="flex-1 overflow-auto p-6 font-mono text-sm text-red-400">
      {{ fatalError }}
    </div>

    <div
      v-else
      class="min-h-0 flex-1 px-2"
      :inert="status !== 'connected'"
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
        class="min-w-[3.25rem] shrink-0 rounded-md bg-neutral-800 px-2 py-2 text-sm active:bg-neutral-600"
        @pointerdown="sendKey(k.bytes, $event)"
      >
        {{ k.label }}
      </button>
    </div>
  </div>
</template>
