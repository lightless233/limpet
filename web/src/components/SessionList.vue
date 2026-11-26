<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, toRaw, useTemplateRef, watch } from 'vue'
import type { useSessions } from '../sessions'
import AppIcon from './AppIcon.vue'

const props = defineProps<{ store: ReturnType<typeof useSessions>; activeId?: string }>()
const emit = defineEmits<{ open: [id: string, name: string] }>()
const { sessions, loading, loadError, authExpired, request, refresh } = props.store
const busy = ref(false)
const closing = ref(new Set<string>())
const closeError = ref('')

const cwdTip = useTemplateRef<HTMLDivElement>('cwdTip')
const cwdTipText = ref('')
const cwdTipPos = ref({ left: 0, top: 0 })
let showTipTimer = 0
let hideTipTimer = 0
let tipSeq = 0

function hideCwdTip() {
  tipSeq++
  clearTimeout(showTipTimer)
  clearTimeout(hideTipTimer)
  showTipTimer = 0
  hideTipTimer = 0
  cwdTipText.value = ''
  const el = cwdTip.value
  if (el?.matches(':popover-open')) {
    try { el.hidePopover() } catch {}
  }
}

function cancelHideCwdTip() {
  clearTimeout(hideTipTimer)
  hideTipTimer = 0
}

function scheduleHideCwdTip() {
  tipSeq++
  clearTimeout(showTipTimer)
  showTipTimer = 0
  clearTimeout(hideTipTimer)
  hideTipTimer = window.setTimeout(hideCwdTip, 120)
}

function showCwdTip(e: PointerEvent, cwd: string) {
  if (e.pointerType && e.pointerType !== 'mouse') return
  const target = e.currentTarget as HTMLElement | null
  if (!target) return
  cancelHideCwdTip()
  clearTimeout(showTipTimer)
  const seq = ++tipSeq
  showTipTimer = window.setTimeout(async () => {
    showTipTimer = 0
    if (seq !== tipSeq || !target.isConnected) return
    cwdTipText.value = cwd
    await nextTick()
    const el = cwdTip.value
    if (seq !== tipSeq || !el || !target.isConnected) return
    const rect = target.getBoundingClientRect()
    el.style.visibility = 'hidden'
    if (!el.matches(':popover-open')) {
      try { el.showPopover() } catch {
        el.style.visibility = ''
        return
      }
    }
    const tipRect = el.getBoundingClientRect()
    const gap = 6
    const margin = 8
    let top = rect.bottom + gap
    if (top + tipRect.height > window.innerHeight - margin && rect.top - tipRect.height - gap >= margin) {
      top = rect.top - tipRect.height - gap
    }
    const left = Math.max(margin, Math.min(rect.left, window.innerWidth - tipRect.width - margin))
    cwdTipPos.value = { left, top }
    el.style.visibility = ''
  }, 180)
}

function onWindowKeydown(e: KeyboardEvent) {
  if (e.key === 'Escape') hideCwdTip()
}

onMounted(() => window.addEventListener('keydown', onWindowKeydown))
onBeforeUnmount(() => {
  window.removeEventListener('keydown', onWindowKeydown)
  hideCwdTip()
})

type Action = { mode: 'create'; name: string } | { mode: 'rename' | 'close'; id: string; name: string }
const action = ref<Action | null>(null)
const actionError = ref('')
const dialog = useTemplateRef('dialog')
const actionTitle = computed(() => {
  switch (action.value?.mode) {
    case 'create': return '新建会话'
    case 'rename': return '重命名会话'
    case 'close': return '关闭会话'
    default: return ''
  }
})

function reloadPage() {
  location.reload()
}

async function openAction(next: Action) {
  hideCwdTip()
  if (busy.value || authExpired.value || ('id' in next && closing.value.has(next.id))) return
  action.value = next
  actionError.value = ''
  await nextTick()
  if (toRaw(action.value) === next && !dialog.value?.open) dialog.value?.showModal()
}

function openCreate() {
  void openAction({ mode: 'create', name: '' })
}

defineExpose({ openCreate })

let outsidePointer: number | null = null
function isOutside(e: PointerEvent) {
  const bounds = dialog.value!.getBoundingClientRect()
  return e.clientX < bounds.left || e.clientX > bounds.right || e.clientY < bounds.top || e.clientY > bounds.bottom
}
const lightDismiss = 'closedBy' in HTMLDialogElement.prototype ? {} : {
  pointerdown(e: PointerEvent) {
    outsidePointer = e.isPrimary && e.button === 0 && isOutside(e) ? e.pointerId : null
  },
  pointerup(e: PointerEvent) {
    if (outsidePointer === e.pointerId && isOutside(e)) dialog.value?.close()
    outsidePointer = null
  },
  pointercancel() {
    outsidePointer = null
  },
}

function onDialogClose() {
  const el = dialog.value
  if (!el || el.open) return
  el.returnValue = ''
  outsidePointer = null
  action.value = null
}

watch(authExpired, (expired) => {
  if (expired) dialog.value?.close()
})

function preventComposingSubmit(e: KeyboardEvent) {
  if (e.isComposing || e.keyCode === 229) e.preventDefault()
}

async function submitAction() {
  const current = action.value
  if (!current || !dialog.value?.open || busy.value || authExpired.value) return
  if (current.mode === 'close') {
    dialog.value?.close()
    await closeSession(current.id)
    return
  }
  if (current.mode === 'rename' && closing.value.has(current.id)) return
  busy.value = true
  actionError.value = ''
  try {
    const url = current.mode === 'create' ? '/api/term' : '/api/term/' + encodeURIComponent(current.id) + '/rename'
    const res = await request(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: current.name.trim() }),
    })
    if (!res) return
    if (!res.ok) {
      const data = await res.json().catch(() => ({}))
      throw new Error(data.error ?? 'HTTP ' + res.status)
    }
    if (action.value === current) dialog.value?.close()
    await refresh()
  } catch (e) {
    actionError.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}

async function closeSession(id: string) {
  if (closing.value.has(id) || authExpired.value) return
  closing.value.add(id)
  closeError.value = ''
  try {
    const res = await request('/api/term/' + encodeURIComponent(id) + '/close', { method: 'POST' })
    if (!res) return
    if (!res.ok) {
      const data = await res.json().catch(() => ({}))
      throw new Error(data.error ?? 'HTTP ' + res.status)
    }
    sessions.value = sessions.value.filter((item) => item.id !== id)
    await refresh()
  } catch (e) {
    closeError.value = '关闭失败：' + (e instanceof Error ? e.message : String(e))
  } finally {
    closing.value.delete(id)
  }
}

function fmtTime(iso: string) {
  const d = new Date(iso)
  const now = new Date()
  const hm = String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0')
  return d.toDateString() === now.toDateString()
    ? '今天 ' + hm
    : (d.getMonth() + 1) + '/' + d.getDate() + ' ' + hm
}
</script>

<template>
  <aside aria-label="会话列表" class="session-list min-h-0 min-w-0 flex-col bg-[var(--limpet-surface)] lg:border-r lg:border-[var(--limpet-line)]">
    <header class="flex shrink-0 items-center justify-between gap-2 border-b border-[var(--limpet-line)] px-3 py-2">
      <h1 class="flex items-center gap-2 text-lg font-semibold tracking-tight">
        <AppIcon name="terminal" class="text-[var(--limpet-accent)]" /> Limpet
      </h1>
      <button v-if="!authExpired" class="control text-sm" @click="openCreate">
        <AppIcon name="add" /> 新建
      </button>
    </header>

    <div v-if="authExpired" class="min-h-0 flex-1 space-y-2 overflow-y-auto px-3 py-3">
      <p role="alert" class="text-sm text-[var(--limpet-danger)]">登录已失效</p>
      <button class="control text-sm" @click="reloadPage">刷新页面</button>
    </div>

    <div v-else class="min-h-0 flex-1 overflow-y-auto overscroll-contain px-2 py-2" @scroll.passive="hideCwdTip">
      <p v-if="closeError" role="alert" class="p-2 text-sm text-[var(--limpet-danger)]">{{ closeError }}</p>
      <p v-if="loadError" role="alert" class="p-2 text-sm text-[var(--limpet-danger)]">
        {{ loadError }} <button class="control" @click="refresh">重试</button>
      </p>
      <p v-else-if="loading && !sessions.length" class="p-2 text-sm text-[var(--limpet-muted)]">加载中…</p>
      <p v-else-if="!sessions.length" class="p-2 text-sm text-[var(--limpet-muted)]">还没有会话</p>

      <ul class="grid gap-1">
        <li v-for="s in sessions" :key="s.id" class="session-row flex min-w-0 items-center gap-1 rounded-md px-1"
          :class="{ 'session-row-active': activeId === s.id }">
          <button
            class="session-entry flex min-h-11 min-w-0 flex-1 items-start gap-2 rounded-md px-2 py-2 text-left disabled:opacity-50"
            :aria-current="activeId === s.id ? 'true' : undefined"
            :disabled="closing.has(s.id)"
            @click="emit('open', s.id, s.name)"
          >
            <span class="mt-1.5 size-2 shrink-0 rounded-full" :class="s.attached ? 'bg-[var(--limpet-accent)]' : 'bg-[var(--limpet-muted)]'"
              :title="s.attached ? '使用中' : '空闲'"></span>
            <span class="grid min-w-0 flex-1 gap-0.5">
              <span class="truncate text-sm font-medium">{{ s.name }}</span>
              <span v-if="s.cmd || s.cwd" class="flex min-w-0 items-center gap-1.5 text-xs text-[var(--limpet-muted)] [font-family:var(--limpet-mono)]">
                <span v-if="s.cmd" class="max-w-[6rem] shrink-0 truncate rounded border border-[var(--limpet-line)] bg-[var(--limpet-canvas)] px-1.5">{{ s.cmd }}</span>
                <span v-if="s.cwd" class="cwd truncate"
                  @pointerenter="showCwdTip($event, s.cwd)" @pointerleave="scheduleHideCwdTip"><bdi dir="ltr">{{ s.cwd }}</bdi></span>
              </span>
              <time :datetime="s.created" class="text-xs text-[var(--limpet-muted)]">{{ fmtTime(s.created) }}</time>
            </span>
          </button>
          <button class="control shrink-0 bg-transparent text-[var(--limpet-muted)] hover:bg-[var(--limpet-pressed)] hover:text-[var(--limpet-text)] active:bg-[var(--limpet-pressed)]" aria-label="重命名" title="重命名"
            :disabled="closing.has(s.id)" @click="openAction({ mode: 'rename', id: s.id, name: s.name })">
            <AppIcon name="edit" />
          </button>
          <button class="control shrink-0 bg-transparent text-[var(--limpet-muted)] hover:bg-[var(--limpet-pressed)] hover:text-[var(--limpet-danger)] active:bg-[var(--limpet-pressed)]" aria-label="关闭" title="关闭"
            :disabled="closing.has(s.id)" @click="openAction({ mode: 'close', id: s.id, name: s.name })">
            <AppIcon name="close" />
          </button>
        </li>
      </ul>
    </div>

    <dialog ref="dialog" closedby="any" class="session-dialog" aria-labelledby="session-action-title"
      @close="onDialogClose" v-on="lightDismiss">
      <form v-if="action" class="grid gap-3" @submit.prevent="submitAction">
        <h2 id="session-action-title" class="text-base font-semibold">{{ actionTitle }}</h2>
        <p v-if="action.mode === 'close'" class="min-w-0 wrap-anywhere text-sm text-[var(--limpet-muted)]">
          关闭「{{ action.name }}」会终止其中的进程。
        </p>
        <input v-else v-model="action.name" autofocus autocomplete="off" spellcheck="false" enterkeyhint="done"
          class="min-w-0 rounded-md border border-[var(--limpet-line)] bg-[var(--limpet-canvas)] px-3 py-2 text-base focus-visible:outline-[var(--limpet-accent)]"
          aria-label="会话名" :placeholder="action.mode === 'create' ? '会话名（留空自动生成）' : '输入新的会话名'"
          @keydown.enter="preventComposingSubmit" />
        <p v-if="actionError" role="alert" class="text-sm text-[var(--limpet-danger)]">{{ actionError }}</p>
        <div class="flex justify-end gap-2 text-sm">
          <button v-if="action.mode === 'close'" type="button" class="control" autofocus @click="dialog?.close()">取消</button>
          <button v-else type="button" class="control" @click="dialog?.close()">取消</button>
          <button type="submit" class="control" :class="action.mode === 'close' ? 'control-danger' : 'control-primary'" :disabled="busy">
            {{ action.mode === 'close' ? '确认关闭' : '确定' }}
          </button>
        </div>
      </form>
    </dialog>

    <div
      ref="cwdTip"
      :popover="'hint' as any"
      role="tooltip"
      dir="ltr"
      class="cwd-tooltip"
      :style="{ left: cwdTipPos.left + 'px', top: cwdTipPos.top + 'px' }"
      @pointerenter="cancelHideCwdTip"
      @pointerleave="scheduleHideCwdTip"
    >
      {{ cwdTipText }}
    </div>
  </aside>
</template>

<style scoped>
.session-list {
  padding: env(safe-area-inset-top) env(safe-area-inset-right) env(safe-area-inset-bottom) env(safe-area-inset-left);
}

.session-row { transition: background-color 120ms ease; }
.session-row:hover { background: rgb(255 255 255 / 4%); }
.session-row-active,
.session-row-active:hover {
  background: var(--limpet-control);
  box-shadow: inset 2px 0 0 var(--limpet-accent);
}
.cwd { direction: rtl; }
.cwd-tooltip {
  position: fixed;
  inset: auto;
  margin: 0;
  max-width: min(28rem, calc(100vw - 16px));
  padding: 0.375rem 0.5rem;
  border: 1px solid var(--limpet-line);
  border-radius: 0.375rem;
  background: var(--limpet-surface);
  color: var(--limpet-text);
  font-family: var(--limpet-mono);
  font-size: 0.75rem;
  line-height: 1.4;
  word-break: break-all;
  box-shadow: 0 8px 24px rgb(0 0 0 / 45%);
}
.session-dialog {
  margin: auto;
  width: min(26rem, calc(100% - max(1rem, env(safe-area-inset-left)) - max(1rem, env(safe-area-inset-right))));
  max-height: calc(100% - max(1rem, env(safe-area-inset-top)) - max(1rem, env(safe-area-inset-bottom)));
  padding: 1rem;
  overflow: auto;
  border: 1px solid var(--limpet-line);
  border-radius: 0.75rem;
  background: var(--limpet-surface);
  color: var(--limpet-text);
}

.session-dialog:modal { overscroll-behavior: contain; }
.session-dialog::backdrop { background: rgb(0 0 0 / 65%); overflow: hidden; overscroll-behavior: contain; }
</style>
