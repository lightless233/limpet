<script setup lang="ts">
// 终端页：wterm(ghostty core) ↔ WebSocket ↔ Go(PTY)。
// 帧约定与后端一致：二进制帧 = 终端 I/O，文本帧 = 控制 JSON。
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { useTemplateRef } from 'vue'
import { Terminal, type WTerm } from '@wterm/vue'
import { GhosttyCore } from '@wterm/ghostty'
import '@wterm/vue/css'

const term = useTemplateRef('term')
const core = ref<GhosttyCore | null>(null)
const loadError = ref('')

type Status = 'connecting' | 'connected' | 'closed' | 'error'
const status = ref<Status>('connecting')
const sessionId = ref('')

const encoder = new TextEncoder()
let ws: WebSocket | null = null
// ready 之前到达的输出先排队，避免丢首屏（提示符、banner）。
const pending: Uint8Array[] = []
let ready = false
// autoResize 的首次回调早于 WS 握手完成，必须把尺寸存下来，
// 在 onopen 时补发，否则 PTY 永远停在初始 80x24：
// zsh 的 PROMPT_SP 标记行按错误列数填充，会在网格里提前换行留下可见的 %。
let lastSize: { cols: number; rows: number } | null = null

onMounted(async () => {
  try {
    core.value = await GhosttyCore.load()
  } catch (e) {
    loadError.value = e instanceof Error ? e.message : String(e)
    status.value = 'error'
    return
  }
  connect()
})

onBeforeUnmount(() => {
  ws?.close()
  ws = null
})

async function connect() {
  ws?.close()
  ready = false
  pending.length = 0
  status.value = 'connecting'

  try {
    const res = await fetch('/api/term', { method: 'POST' })
    if (!res.ok) throw new Error(`创建会话失败: HTTP ${res.status}`)
    const data: { id: string } = await res.json()
    sessionId.value = data.id

    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const sizeQuery = lastSize ? `?cols=${lastSize.cols}&rows=${lastSize.rows}` : ''
    ws = new WebSocket(`${proto}//${location.host}/api/ws/term/${data.id}${sizeQuery}`)
    ws.binaryType = 'arraybuffer'
    ws.onopen = () => {
      status.value = 'connected'
      sendResize()
    }
    ws.onmessage = (e) => {
      const bytes = new Uint8Array(e.data as ArrayBuffer)
      if (!ready) {
        pending.push(bytes)
      } else {
        term.value?.write(bytes)
      }
    }
    ws.onclose = () => {
      if (status.value !== 'error') status.value = 'closed'
    }
  } catch (e) {
    loadError.value = e instanceof Error ? e.message : String(e)
    status.value = 'error'
  }
}

function flushPending() {
  for (const bytes of pending) term.value?.write(bytes)
  pending.length = 0
}

function onData(chunk: string) {
  if (ws?.readyState === WebSocket.OPEN) ws.send(encoder.encode(chunk))
}

// 终端程序开启鼠标跟踪后的原始 X10 报告。
function onBinary(bytes: Uint8Array) {
  if (ws?.readyState === WebSocket.OPEN) ws.send(new Uint8Array(bytes))
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
  ready = true
  flushPending()
  wt.focus()
}
</script>

<template>
  <div class="flex h-dvh flex-col bg-neutral-950 text-neutral-200">
    <header class="flex items-center gap-3 px-4 py-2 text-sm">
      <span class="font-semibold tracking-tight">Limpet</span>
      <span
        class="rounded-full px-2 py-0.5 text-xs"
        :class="{
          'bg-neutral-800 text-neutral-400': status === 'connecting',
          'bg-emerald-900/60 text-emerald-400': status === 'connected',
          'bg-amber-900/60 text-amber-400': status === 'closed',
          'bg-red-900/60 text-red-400': status === 'error',
        }"
      >
        {{ status === 'connecting' && sessionId ? '接入中' : { connecting: '连接中', connected: '已连接', closed: '已断开', error: '错误' }[status] }}
      </span>
      <span v-if="sessionId" class="font-mono text-xs text-neutral-500">{{ sessionId }}</span>
      <button
        v-if="status === 'closed' || status === 'error'"
        class="ml-auto rounded-md bg-neutral-800 px-3 py-1 text-xs hover:bg-neutral-700 active:scale-95"
        @click="connect"
      >
        重新连接
      </button>
    </header>

    <div v-if="loadError" class="flex-1 overflow-auto p-6 font-mono text-sm text-red-400">
      {{ loadError }}
    </div>

    <div v-else class="min-h-0 flex-1 px-2 pb-2">
      <Terminal
        v-if="core"
        ref="term"
        :core="core"
        auto-resize
        class="h-full w-full"
        @data="onData"
        @binary="onBinary"
        @resize="onResize"
        @ready="onReady"
        @error="(e: unknown) => { loadError = String(e); status = 'error' }"
      />
    </div>
  </div>
</template>
