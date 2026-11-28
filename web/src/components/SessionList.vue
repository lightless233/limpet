<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, toRaw, useId, useTemplateRef, watch } from 'vue'
import { groupSessions, workspaceKey, type useSessions } from '../sessions'
import AppIcon from './AppIcon.vue'

const props = defineProps<{ store: ReturnType<typeof useSessions>; activeId?: string }>()
const emit = defineEmits<{ open: [id: string, name: string] }>()
const { sessions, loading, refreshing, loadError, authExpired, request, refresh } = props.store
const busy = ref(false)
const closing = ref(new Set<string>())
const closeError = ref('')
const createError = ref('')
const creating = ref(new Set<string>())
const workspaces = computed(() => groupSessions(sessions.value))
const collapsedKey = 'limpet.collapsed-workspaces'
const collapsed = ref(new Set<string>())
try {
  const saved: unknown = JSON.parse(localStorage.getItem(collapsedKey) ?? '[]')
  if (Array.isArray(saved)) collapsed.value = new Set(saved.filter((key): key is string => typeof key === 'string'))
} catch { /* Use the default expanded state when storage is unavailable. */ }

function setCollapsed(key: string, next: boolean) {
  if (collapsed.value.has(key) === next) return
  if (next) collapsed.value.add(key)
  else collapsed.value.delete(key)
  try {
    localStorage.setItem(collapsedKey, JSON.stringify([...collapsed.value]))
  } catch { /* The current page still keeps its collapse state. */ }
}

const activeWorkspace = computed(() => sessions.value.find((item) => item.id === props.activeId))
watch([() => props.activeId, () => activeWorkspace.value && workspaceKey(activeWorkspace.value.cwd)], ([id, key]) => {
  if (id && key !== undefined) setCollapsed(key, false)
})

const presets = [
  { id: 'shell', label: 'Shell', flag: '' },
  { id: 'codex', label: 'Codex', flag: '--yolo' },
  { id: 'agy', label: 'Agy', flag: '--dangerously-skip-permissions' },
] as const
type Preset = typeof presets[number]['id']
type CreatedSession = { id: string; name: string; cwd: string; cmd: string }
const workspaceHistory = ref<string[]>([])
const presetMenuId = useId()
const workspaceListId = useId()
const presetMenu = useTemplateRef<HTMLDivElement>('presetMenu')
const menuWorkspace = ref<string | null>(null)

function preparePresetMenu(key: string | null) {
  hideCwdTip()
  menuWorkspace.value = key
}

async function requestCreate(name: string, cwd: string, preset: Preset): Promise<CreatedSession | undefined> {
  const res = await request('/api/term', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: name.trim(), cwd, preset }),
  })
  if (!res) return
  if (!res.ok) {
    const data = await res.json().catch(() => ({}))
    throw new Error(data.error ?? 'HTTP ' + res.status)
  }
  return res.json()
}

function openCreated(created: CreatedSession) {
  setCollapsed(workspaceKey(created.cwd), false)
  emit('open', created.id, created.name)
  void refresh()
}

async function selectPreset(preset: Preset) {
  const cwd = menuWorkspace.value
  presetMenu.value?.hidePopover()
  if (cwd === null) {
    openCreate(preset)
    return
  }
  if (creating.value.has(cwd) || authExpired.value) return
  creating.value.add(cwd)
  createError.value = ''
  try {
    const created = await requestCreate('', cwd, preset)
    if (created) openCreated(created)
  } catch (error) {
    createError.value = '新建失败：' + (error instanceof Error ? error.message : String(error))
  } finally {
    creating.value.delete(cwd)
  }
}

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

type Action = { mode: 'create'; name: string; cwd: string; preset: Preset } |
  { mode: 'rename' | 'close'; id: string; name: string }
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

async function openCreate(preset: Preset = 'shell') {
  const next: Action = { mode: 'create', name: '', cwd: '', preset }
  workspaceHistory.value = []
  await openAction(next)
  if (toRaw(action.value) !== next) return
  try {
    const res = await request('/api/workspaces')
    if (!res?.ok) return
    const paths: string[] = await res.json()
    if (toRaw(action.value) === next) workspaceHistory.value = paths
  } catch { /* Workspace history is optional. */ }
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
    if (current.mode === 'create') {
      const created = await requestCreate(current.name, current.cwd, current.preset)
      if (created) {
        if (action.value === current) dialog.value?.close()
        openCreated(created)
      }
      return
    }
    const res = await request('/api/term/' + encodeURIComponent(current.id) + '/rename', {
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
      <div v-if="!authExpired" class="flex items-center gap-1">
        <button class="control" aria-label="刷新列表" title="刷新列表" :disabled="refreshing" @click="refresh()">
          <AppIcon name="refresh" :class="{ 'animate-spin': refreshing }" />
        </button>
        <div class="inline-flex gap-px">
          <button class="control rounded-e-none text-sm" @click="openCreate()">
            <AppIcon name="add" /> 新建
          </button>
          <button class="control rounded-s-none" aria-label="更多新建选项" :popovertarget="presetMenuId"
            @click="preparePresetMenu(null)">
            <AppIcon name="down" />
          </button>
        </div>
      </div>
    </header>

    <div v-if="authExpired" class="min-h-0 flex-1 space-y-2 overflow-y-auto px-3 py-3">
      <p role="alert" class="text-sm text-[var(--limpet-danger)]">登录已失效</p>
      <button class="control text-sm" @click="reloadPage">刷新页面</button>
    </div>

    <div v-else class="session-groups min-h-0 min-w-0 flex-1 overflow-y-auto overscroll-contain px-2 py-2" @scroll.passive="hideCwdTip">
      <p v-if="createError" role="alert" class="wrap-anywhere p-2 text-sm text-[var(--limpet-danger)]">{{ createError }}</p>
      <p v-if="closeError" role="alert" class="wrap-anywhere p-2 text-sm text-[var(--limpet-danger)]">{{ closeError }}</p>
      <p v-if="loadError" role="alert" class="p-2 text-sm text-[var(--limpet-danger)]">
        {{ loadError }} <button class="control" @click="refresh()">重试</button>
      </p>
      <p v-else-if="loading && !sessions.length" class="p-2 text-sm text-[var(--limpet-muted)]">加载中…</p>
      <p v-else-if="!sessions.length" class="p-2 text-sm text-[var(--limpet-muted)]">还没有会话</p>

      <section v-for="(group, index) in workspaces" :key="group.key" class="workspace-group mb-2 min-w-0">
        <div class="workspace-heading flex min-w-0 items-center gap-1">
          <h2 class="min-w-0 flex-1">
            <button type="button" class="control w-full min-w-0 justify-start bg-transparent text-left text-xs"
              :aria-expanded="!collapsed.has(group.key)" :aria-controls="workspaceListId + '-' + index"
              @click="setCollapsed(group.key, !collapsed.has(group.key))">
              <AppIcon name="down" class="size-3 shrink-0 text-[var(--limpet-muted)]" :class="{ '-rotate-90': collapsed.has(group.key) }" />
              <bdi class="min-w-0 flex-1 truncate font-semibold" dir="ltr"
                @pointerenter="showCwdTip($event, group.key || '~')" @pointerleave="scheduleHideCwdTip">{{ group.title }}</bdi>
              <span class="shrink-0 tabular-nums text-[var(--limpet-muted)]">{{ group.items.length }}</span>
            </button>
          </h2>
          <button type="button" class="control shrink-0 bg-transparent text-[var(--limpet-muted)]"
            :aria-label="'在 ' + group.title + ' 新建会话'" :popovertarget="presetMenuId"
            :aria-busy="creating.has(group.key)" :disabled="creating.has(group.key)" @click="preparePresetMenu(group.key)">
            <AppIcon v-if="creating.has(group.key)" name="refresh" class="animate-spin" />
            <AppIcon v-else name="add" />
          </button>
        </div>
        <ul v-show="!collapsed.has(group.key)" :id="workspaceListId + '-' + index" class="grid min-w-0 gap-0.5">
          <li v-for="s in group.items" :key="s.id" class="session-row flex min-w-0 items-center gap-1 rounded-md px-1"
            :class="{ 'session-row-active': activeId === s.id }">
            <button
              class="session-entry grid min-h-11 min-w-0 flex-1 content-center gap-0.5 rounded-md px-2 py-1.5 text-left disabled:opacity-50"
              :aria-current="activeId === s.id ? 'true' : undefined"
              :disabled="closing.has(s.id)"
              @click="emit('open', s.id, s.name)"
            >
              <span class="flex min-w-0 items-center gap-2">
                <span class="size-1.5 shrink-0 rounded-full" :class="s.attached ? 'bg-[var(--limpet-accent)]' : 'bg-[var(--limpet-muted)]'"
                  :title="s.attached ? '使用中' : '空闲'"></span>
                <span class="min-w-0 flex-1 truncate text-sm font-medium">{{ s.name }}</span>
              </span>
              <span class="flex min-w-0 items-center gap-1.5 pl-3.5 text-xs text-[var(--limpet-muted)]">
                <span v-if="s.cmd" :data-command="s.cmd" class="command-badge min-w-0 max-w-[6rem] truncate rounded border px-1 [font-family:var(--limpet-mono)]">{{ s.cmd }}</span>
                <time :datetime="s.created" class="shrink-0">{{ fmtTime(s.created) }}</time>
              </span>
            </button>
            <button class="control session-row-action shrink-0 bg-transparent text-[var(--limpet-muted)] hover:bg-[var(--limpet-pressed)] hover:text-[var(--limpet-text)] active:bg-[var(--limpet-pressed)]" aria-label="重命名" title="重命名"
              :disabled="closing.has(s.id)" @click="openAction({ mode: 'rename', id: s.id, name: s.name })">
              <AppIcon name="edit" />
            </button>
            <button class="control session-row-action shrink-0 bg-transparent text-[var(--limpet-muted)] hover:bg-[var(--limpet-pressed)] hover:text-[var(--limpet-danger)] active:bg-[var(--limpet-pressed)]" aria-label="关闭" title="关闭"
              :disabled="closing.has(s.id)" @click="openAction({ mode: 'close', id: s.id, name: s.name })">
              <AppIcon name="close" />
            </button>
          </li>
        </ul>
      </section>
    </div>

    <div v-if="!authExpired" :id="presetMenuId" ref="presetMenu" popover="auto" class="preset-popover">
      <div role="group" aria-label="启动预设" class="grid gap-1">
        <button v-for="preset in presets" :key="preset.id" type="button"
          class="control min-w-0 justify-start px-3 text-left text-sm" @click="selectPreset(preset.id)">
          <span class="shrink-0">{{ preset.label }}</span>
          <span v-if="preset.flag" class="min-w-0 wrap-anywhere text-xs text-[var(--limpet-muted)] [font-family:var(--limpet-mono)]">{{ preset.flag }}</span>
        </button>
      </div>
    </div>

    <dialog ref="dialog" closedby="any" class="session-dialog" aria-labelledby="session-action-title"
      @close="onDialogClose" v-on="lightDismiss">
      <form v-if="action" class="grid min-w-0 grid-cols-[minmax(0,1fr)] gap-3" @submit.prevent="submitAction">
        <h2 id="session-action-title" class="text-base font-semibold">{{ actionTitle }}</h2>
        <p v-if="action.mode === 'close'" class="min-w-0 wrap-anywhere text-sm text-[var(--limpet-muted)]">
          关闭「{{ action.name }}」会终止其中的进程。
        </p>
        <template v-if="action.mode === 'create'">
          <fieldset class="grid min-w-0 grid-cols-3 gap-1 text-sm">
            <legend class="sr-only">启动预设</legend>
            <label v-for="preset in presets" :key="preset.id" class="control preset-choice cursor-pointer">
              <input v-model="action.preset" type="radio" name="preset" :value="preset.id" class="sr-only" />
              {{ preset.label }}
            </label>
          </fieldset>
          <input v-model="action.cwd" autocomplete="off" spellcheck="false" aria-label="工作目录" placeholder="~"
            class="min-w-0 rounded-md border border-[var(--limpet-line)] bg-[var(--limpet-canvas)] px-3 py-2 text-base focus-visible:outline-[var(--limpet-accent)]"
            @keydown.enter="preventComposingSubmit" />
          <div v-if="workspaceHistory.length" class="flex min-w-0 flex-wrap gap-1">
            <button v-for="path in workspaceHistory" :key="path" type="button"
              class="control max-w-full justify-start text-xs" @click="action.cwd = path">
              <bdi class="min-w-0 truncate" dir="ltr">{{ path }}</bdi>
            </button>
          </div>
        </template>
        <input v-if="action.mode !== 'close'" v-model="action.name" autofocus autocomplete="off" spellcheck="false" enterkeyhint="done"
          class="min-w-0 rounded-md border border-[var(--limpet-line)] bg-[var(--limpet-canvas)] px-3 py-2 text-base focus-visible:outline-[var(--limpet-accent)]"
          aria-label="会话名" :placeholder="action.mode === 'create' ? '会话名（留空自动生成）' : '输入新的会话名'"
          @keydown.enter="preventComposingSubmit" />
        <p v-if="actionError" role="alert" class="wrap-anywhere text-sm text-[var(--limpet-danger)]">{{ actionError }}</p>
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
.workspace-heading {
  border-bottom: 1px solid var(--limpet-line);
  margin-bottom: 0.25rem;
}
.command-badge {
  border-color: var(--limpet-line);
  background: var(--limpet-canvas);
}
.command-badge[data-command="codex"] {
  color: #b4abd1;
  background: #232031;
  border-color: #322b42;
}
.command-badge[data-command="agy"] {
  color: #a5c4ba;
  background: #1c2928;
  border-color: #2a3b38;
}
@media (hover: hover) and (pointer: fine) {
  .session-row-action { opacity: 0; }
  .session-row:is(:hover, :focus-within, .session-row-active) .session-row-action { opacity: 1; }
}
@media (prefers-reduced-motion: no-preference) {
  .session-row-action { transition: opacity 120ms ease; }
}
.preset-choice:has(input:checked) {
  color: var(--limpet-accent);
  background: var(--limpet-pressed);
}
.preset-choice:has(input:focus-visible) {
  outline: 2px solid var(--limpet-accent);
  outline-offset: -2px;
}
.preset-popover {
  width: min(17rem, calc(100vw - 1rem));
  max-height: calc(100dvh - 1rem);
  overflow: auto;
  padding: 0.375rem;
  border: 1px solid var(--limpet-line);
  border-radius: 0.5rem;
  background: var(--limpet-surface);
  color: var(--limpet-text);
  box-shadow: 0 8px 24px rgb(0 0 0 / 45%);
}
@supports (position-area: block-end) {
  .preset-popover {
    inset: auto;
    margin: 0;
    position-area: block-end span-inline-start;
    position-try-fallbacks: flip-block, flip-inline, flip-block flip-inline;
  }
}
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
