<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, toRaw, useId, useTemplateRef, watch } from 'vue'
import { groupSessions, TEMP_WORKSPACE_KEY, workspaceKey, type SessionOrder, type useSessions } from '../sessions'
import { resolvedTheme, toggleTheme } from '../theme'
import AppIcon from './AppIcon.vue'

const props = defineProps<{ store: ReturnType<typeof useSessions>; activeId?: string }>()
const emit = defineEmits<{ open: [id: string, name: string] }>()
const { sessions, loading, refreshing, loadError, authExpired, request, refresh, order, saveOrder, resetOrder } = props.store
const busy = ref(false)
const closing = ref(new Set<string>())
const closeError = ref('')
const createError = ref('')
const creating = ref(new Set<string>())
const workspaces = computed(() => groupSessions(sessions.value, order.value, { includeEmptyTemp: true }))

const orderDirty = computed(() => order.value.workspaces.length > 0 || Object.keys(order.value.sessions).length > 0)
const restoring = ref(false)
const orderError = ref('')

async function resetToTimeOrder() {
  if (restoring.value) return
  restoring.value = true
  orderError.value = ''
  try {
    await resetOrder()
  } catch (e) {
    orderError.value = '恢复排序失败：' + (e instanceof Error ? e.message : String(e))
  } finally {
    restoring.value = false
  }
}

// —— 手动排序拖拽（Pointer Events）：仅在专用手柄上响应，touch-action 只禁手柄本身，
// 列表其余区域保持原生滚动。组内拖拽重排会话、组间拖拽重排普通工作区（「临时工作区」
// 不参与组间拖拽，永远沉底）。松手后把完整列表写入 order 并 PUT /api/order 多端同步。
interface DragVisual {
  kind: 'session' | 'workspace'
  groupKey: string // 会话拖拽：所属工作区 key；工作区拖拽：''
  id: string // 会话 ID 或工作区 key
  index: number // 当前插入位置（相对渲染列表，-1 未计算）
  active: boolean // 是否已越过触发阈值
}
const drag = ref<DragVisual | null>(null)
let dragCleanup: (() => void) | null = null

function startDrag(e: PointerEvent, target: { kind: 'session'; groupKey: string; id: string } | { kind: 'workspace'; key: string }) {
  if (authExpired.value) return
  if (e.pointerType === 'mouse' && e.button !== 0) return
  const handle = e.currentTarget as HTMLElement
  // reactive 而非 ref+裸对象：拖动过程中的 index/active 变更必须触发指示线重渲染。
  const visual = reactive<DragVisual>(target.kind === 'session'
    ? { kind: 'session', groupKey: target.groupKey, id: target.id, index: -1, active: false }
    : { kind: 'workspace', groupKey: '', id: target.key, index: -1, active: false })
  const listEl = target.kind === 'session'
    ? handle.closest('ul')
    : (handle.closest('section.workspace-group')?.parentElement ?? null)
  let index = -1
  const onMove = (ev: PointerEvent) => {
    if (ev.pointerId !== e.pointerId) return
    if (!visual.active) {
      if (Math.abs(ev.clientY - e.clientY) < 6) return
      visual.active = true
      drag.value = visual
    }
    index = computeInsertIndex(listEl, ev.clientY, visual.kind)
    visual.index = index
  }
  const cleanup = () => {
    handle.removeEventListener('pointermove', onMove)
    handle.removeEventListener('pointerup', onUp)
    handle.removeEventListener('pointercancel', onCancel)
    handle.removeEventListener('lostpointercapture', onLost)
    drag.value = null
    dragCleanup = null
  }
  const finish = (ev: PointerEvent, commit: boolean) => {
    if (ev.pointerId !== e.pointerId) return
    cleanup()
    if (commit && visual.active && index >= 0) commitDrag(visual, index)
  }
  const onUp = (ev: PointerEvent) => finish(ev, true)
  const onCancel = (ev: PointerEvent) => finish(ev, false)
  const onLost = () => { if (dragCleanup === cleanup) cleanup() }
  handle.addEventListener('pointermove', onMove)
  handle.addEventListener('pointerup', onUp)
  handle.addEventListener('pointercancel', onCancel)
  handle.addEventListener('lostpointercapture', onLost)
  dragCleanup = cleanup
  handle.setPointerCapture(e.pointerId)
}

onBeforeUnmount(() => dragCleanup?.())

// computeInsertIndex 按指针 Y 与各行中点比较，返回「插到第几行之前」（等于行数＝插到最后）。
// 高度为 0 的行（折叠组等）跳过。
function computeInsertIndex(listEl: HTMLElement | null, y: number, kind: 'session' | 'workspace'): number {
  if (!listEl) return -1
  const selector = kind === 'session' ? ':scope > li[data-session-id]' : ':scope > section[data-workspace-key]'
  const rows = [...listEl.querySelectorAll(selector)].filter((el) =>
    kind === 'session' || (el as HTMLElement).dataset.workspaceKey !== TEMP_WORKSPACE_KEY)
  let index = rows.length
  for (let i = 0; i < rows.length; i++) {
    const rect = rows[i].getBoundingClientRect()
    if (rect.height <= 0) continue
    if (y < rect.top + rect.height / 2) {
      index = i
      break
    }
  }
  return index
}

async function saveOrderSafe(next: SessionOrder) {
  orderError.value = ''
  try {
    await saveOrder(next)
  } catch (e) {
    orderError.value = '保存排序失败：' + (e instanceof Error ? e.message : String(e))
  }
}

function commitDrag(visual: DragVisual, index: number) {
  if (visual.kind === 'session') {
    const group = workspaces.value.find((g) => g.key === visual.groupKey)
    if (!group) return
    const ids = group.items.map((item) => item.id)
    const from = ids.indexOf(visual.id)
    if (from < 0) return
    let to = index <= from ? index : index - 1
    to = Math.max(0, Math.min(to, ids.length - 1))
    if (to === from) return
    ids.splice(from, 1)
    ids.splice(to, 0, visual.id)
    void saveOrderSafe({ workspaces: [...order.value.workspaces], sessions: { ...order.value.sessions, [visual.groupKey]: ids } })
    return
  }
  const keys = workspaces.value.filter((g) => g.key !== TEMP_WORKSPACE_KEY).map((g) => g.key)
  const from = keys.indexOf(visual.id)
  if (from < 0) return
  let to = index <= from ? index : index - 1
  to = Math.max(0, Math.min(to, keys.length - 1))
  if (to === from) return
  keys.splice(from, 1)
  keys.splice(to, 0, visual.id)
  void saveOrderSafe({ workspaces: keys, sessions: { ...order.value.sessions } })
}

function sessionBefore(groupKey: string, i: number): boolean {
  const d = drag.value
  return d?.kind === 'session' && d.groupKey === groupKey && d.index === i
}

function sessionAfter(groupKey: string, count: number, i: number): boolean {
  const d = drag.value
  return d?.kind === 'session' && d.groupKey === groupKey && d.index >= count && i === count - 1
}

function isDraggingSession(groupKey: string, id: string): boolean {
  const d = drag.value
  return d?.kind === 'session' && d.groupKey === groupKey && d.id === id
}

function workspaceBefore(groupKey: string, i: number): boolean {
  const d = drag.value
  return d?.kind === 'workspace' && groupKey !== TEMP_WORKSPACE_KEY && d.index === i
}

// 工作区拖拽的「插到最后」指示：普通组数量（渲染列表去掉沉底的临时工作区）。
function workspaceAfter(groupKey: string, i: number, total: number): boolean {
  const d = drag.value
  return d?.kind === 'workspace' && groupKey !== TEMP_WORKSPACE_KEY && d.index >= total - 1 && i === total - 2
}

function isDraggingWorkspace(key: string): boolean {
  const d = drag.value
  return d?.kind === 'workspace' && d.id === key
}
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

type DirEntry = { name: string; path: string }
type DirListing = { path: string; parent: string; dirs: DirEntry[] }
const dirOpen = ref(false)
const dirInput = ref('')
const dirPath = ref('')
const dirParent = ref('')
const dirEntries = ref<DirEntry[]>([])
const dirLoading = ref(false)
const dirError = ref('')
const dirFilter = ref('')
const showHiddenDirs = ref(false)
const dirAddrInput = useTemplateRef<HTMLInputElement>('dirAddrInput')
const newDirOpen = ref(false)
const newDirName = ref('')
const newDirBusy = ref(false)
const newDirError = ref('')
const newDirInput = useTemplateRef<HTMLInputElement>('newDirInput')
let dirReqSeq = 0

const filteredDirEntries = computed(() => {
  const q = dirFilter.value.trim().toLowerCase()
  if (!q) return dirEntries.value
  return dirEntries.value.filter((item) => item.name.toLowerCase().includes(q))
})

function cancelNewDir() {
  newDirOpen.value = false
  newDirName.value = ''
  newDirError.value = ''
  newDirBusy.value = false
}

function resetDirBrowser() {
  dirReqSeq++
  dirOpen.value = false
  dirInput.value = ''
  dirPath.value = ''
  dirParent.value = ''
  dirEntries.value = []
  dirLoading.value = false
  dirError.value = ''
  dirFilter.value = ''
  showHiddenDirs.value = false
  cancelNewDir()
}

async function browseDir(target: string) {
  const seq = ++dirReqSeq
  dirLoading.value = true
  dirError.value = ''
  const params = new URLSearchParams()
  if (target) params.set('path', target)
  if (showHiddenDirs.value) params.set('hidden', '1')
  try {
    const res = await request('/api/dirs?' + params.toString())
    if (seq !== dirReqSeq || !res) return
    if (!res.ok) {
      const data = await res.json().catch(() => ({}))
      throw new Error(data.error ?? 'HTTP ' + res.status)
    }
    const data: DirListing = await res.json()
    if (seq !== dirReqSeq) return
    if (data.path !== dirPath.value) {
      dirFilter.value = ''
      cancelNewDir()
    }
    dirPath.value = data.path
    dirInput.value = data.path
    dirParent.value = data.parent
    dirEntries.value = data.dirs
  } catch (e) {
    if (seq !== dirReqSeq) return
    dirError.value = e instanceof Error ? e.message : String(e)
  } finally {
    if (seq === dirReqSeq) dirLoading.value = false
  }
}

function openDirPicker() {
  if (action.value?.mode !== 'create') return
  const initial = action.value.cwd.trim() || '~'
  dirOpen.value = true
  dirInput.value = initial
  dirFilter.value = ''
  cancelNewDir()
  void browseDir(initial)
  void nextTick(() => dirAddrInput.value?.focus())
}

function closeDirPicker() {
  dirReqSeq++
  dirOpen.value = false
  dirLoading.value = false
  cancelNewDir()
}

function jumpDir() {
  void browseDir(dirInput.value.trim() || '~')
}

function confirmDirPicker() {
  if (action.value?.mode === 'create') {
    const chosen = dirInput.value.trim() || dirPath.value
    if (chosen) action.value.cwd = chosen
  }
  closeDirPicker()
}

function toggleHiddenDirs() {
  showHiddenDirs.value = !showHiddenDirs.value
  void browseDir(dirPath.value || dirInput.value.trim() || '~')
}

function toggleNewDir() {
  if (newDirOpen.value) {
    cancelNewDir()
    return
  }
  if (!dirPath.value || dirLoading.value) return
  newDirOpen.value = true
  newDirName.value = ''
  newDirError.value = ''
  void nextTick(() => newDirInput.value?.focus())
}

async function submitNewDir() {
  if (newDirBusy.value || !dirPath.value) return
  const name = newDirName.value.trim()
  if (!name) {
    newDirError.value = '请输入文件夹名称'
    return
  }
  newDirBusy.value = true
  newDirError.value = ''
  try {
    const res = await request('/api/dirs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ parent: dirPath.value, name }),
    })
    if (!res) return
    if (!res.ok) {
      const data = await res.json().catch(() => ({}))
      throw new Error(data.error ?? 'HTTP ' + res.status)
    }
    const created: DirEntry = await res.json()
    cancelNewDir()
    await browseDir(created.path)
  } catch (e) {
    newDirError.value = e instanceof Error ? e.message : String(e)
  } finally {
    newDirBusy.value = false
  }
}

function onNewDirEnter(e: KeyboardEvent) {
  if (e.isComposing || e.keyCode === 229) return
  e.preventDefault()
  void submitNewDir()
}

function onDialogCancel(e: Event) {
  if (!dirOpen.value) return
  e.preventDefault()
  if (newDirOpen.value) {
    cancelNewDir()
    return
  }
  closeDirPicker()
}

function reloadPage() {
  location.reload()
}

async function openAction(next: Action) {
  hideCwdTip()
  if (busy.value || authExpired.value || ('id' in next && closing.value.has(next.id))) return
  resetDirBrowser()
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
  resetDirBrowser()
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
    <header class="flex min-h-[44px] shrink-0 items-center justify-between gap-2 border-b border-[var(--limpet-line)] px-2.5 py-1">
      <h1 class="flex shrink-0 items-center gap-1.5 text-base font-semibold tracking-tight">
        <AppIcon name="terminal" class="text-[var(--limpet-accent)]" /> Limpet
      </h1>
      <div v-if="!authExpired" class="flex shrink-0 items-center gap-1">
        <button class="control" aria-label="刷新列表" title="刷新列表" :disabled="refreshing" @click="refresh()">
          <AppIcon name="refresh" :class="{ 'animate-spin': refreshing }" />
        </button>
        <button class="control" :aria-label="resolvedTheme === 'dark' ? '切换浅色模式' : '切换深色模式'"
          :title="resolvedTheme === 'dark' ? '切换浅色模式' : '切换深色模式'" @click="toggleTheme">
          <AppIcon :name="resolvedTheme === 'dark' ? 'sun' : 'moon'" />
        </button>
        <div class="inline-flex gap-px">
          <button class="control whitespace-nowrap rounded-e-none px-1.5 text-sm" @click="openCreate()">
            <AppIcon name="add" /> 新建
          </button>
          <button class="control rounded-s-none px-1.5" aria-label="更多新建选项" :popovertarget="presetMenuId"
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

      <div v-if="orderDirty" class="order-restore mx-1 mb-1 flex items-center gap-2 rounded-md px-2 py-1 text-xs">
        <span class="min-w-0 flex-1 truncate text-[var(--limpet-muted)]">已自定义排序</span>
        <button type="button" class="control shrink-0 text-xs" :disabled="restoring" @click="resetToTimeOrder">
          恢复按时间排序
        </button>
      </div>
      <p v-if="orderError" role="alert" class="wrap-anywhere p-2 text-xs text-[var(--limpet-danger)]">{{ orderError }}</p>

      <section v-for="(group, index) in workspaces" :key="group.key" :data-workspace-key="group.key"
        class="workspace-group min-w-0"
        :class="{
          'drag-dragging': isDraggingWorkspace(group.key),
          'drag-target-before': workspaceBefore(group.key, index),
          'drag-target-after': workspaceAfter(group.key, index, workspaces.length),
        }">
        <div class="workspace-heading flex min-w-0 items-center gap-1">
          <h2 class="min-w-0 flex-1">
            <button type="button" class="workspace-toggle control w-full min-w-0 justify-start bg-transparent text-left text-[13px] font-medium hover:bg-[var(--limpet-hover)] active:bg-[var(--limpet-pressed)]"
              :aria-expanded="!collapsed.has(group.key)" :aria-controls="workspaceListId + '-' + index"
              @click="setCollapsed(group.key, !collapsed.has(group.key))">
              <AppIcon :name="collapsed.has(group.key) ? 'folder' : 'folderOpen'" class="size-4 shrink-0 text-[var(--limpet-muted)]" />
              <bdi class="min-w-0 flex-1 truncate" dir="ltr"
                @pointerenter="showCwdTip($event, group.key || '~')" @pointerleave="scheduleHideCwdTip">{{ group.title }}</bdi>
              <span class="shrink-0 text-xs tabular-nums text-[var(--limpet-muted)]">{{ group.items.length }}</span>
              <AppIcon name="down" class="size-3 shrink-0 text-[var(--limpet-muted)] transition-transform duration-150" :class="{ '-rotate-90': collapsed.has(group.key) }" />
            </button>
          </h2>
          <button v-if="group.key !== TEMP_WORKSPACE_KEY" type="button"
            class="control drag-handle shrink-0 bg-transparent text-[var(--limpet-muted)]"
            aria-label="拖动调整工作区顺序" title="拖动调整顺序"
            @pointerdown="startDrag($event, { kind: 'workspace', key: group.key })">
            <AppIcon name="grip" />
          </button>
          <button type="button" class="control shrink-0 bg-transparent text-[var(--limpet-muted)] hover:bg-[var(--limpet-hover)] hover:text-[var(--limpet-text)] active:bg-[var(--limpet-pressed)]"
            :aria-label="'在 ' + group.title + ' 新建会话'" :popovertarget="presetMenuId"
            :aria-busy="creating.has(group.key)" :disabled="creating.has(group.key)" @click="preparePresetMenu(group.key)">
            <AppIcon v-if="creating.has(group.key)" name="refresh" class="animate-spin" />
            <AppIcon v-else name="add" />
          </button>
        </div>
        <ul v-show="!collapsed.has(group.key)" :id="workspaceListId + '-' + index" class="grid min-w-0 gap-0.5 pl-2">
          <li v-for="(s, si) in group.items" :key="s.id" :data-session-id="s.id"
            class="session-row flex min-w-0 items-center gap-0.5 rounded-md px-1"
            :class="{
              'session-row-active': activeId === s.id,
              'drag-dragging': isDraggingSession(group.key, s.id),
              'drag-target-before': sessionBefore(group.key, si),
              'drag-target-after': sessionAfter(group.key, group.items.length, si),
            }">
            <button type="button" class="control drag-handle shrink-0 bg-transparent text-[var(--limpet-muted)]"
              aria-label="拖动调整会话顺序" title="拖动调整顺序"
              @pointerdown="startDrag($event, { kind: 'session', groupKey: group.key, id: s.id })">
              <AppIcon name="grip" />
            </button>
            <button
              class="session-entry grid min-h-10 min-w-0 flex-1 content-center gap-0.5 rounded-md px-1.5 py-1 text-left disabled:opacity-50"
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
                <span v-if="s.cmd" :data-command="s.cmd" class="command-badge shrink-0 max-w-[6rem] truncate rounded border px-1.5 font-medium [font-family:var(--limpet-mono)]">{{ s.cmd }}</span>
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
      @cancel="onDialogCancel" @close="onDialogClose" v-on="lightDismiss">
      <div v-if="action?.mode === 'create' && dirOpen" class="grid min-w-0 grid-cols-[minmax(0,1fr)] gap-3">
        <div class="flex min-w-0 items-center justify-between gap-2">
          <div class="flex min-w-0 items-center gap-1.5">
            <button type="button" class="control shrink-0" aria-label="返回新建会话" title="返回" @click="closeDirPicker">
              <AppIcon name="back" class="size-4" />
            </button>
            <h2 id="session-action-title" class="truncate text-base font-semibold">选择文件夹</h2>
          </div>
          <button type="button" class="control shrink-0 bg-transparent text-[var(--limpet-muted)] hover:text-[var(--limpet-text)]"
            aria-label="关闭窗口" title="关闭" @click="dialog?.close()">
            <AppIcon name="close" class="size-4" />
          </button>
        </div>

        <form class="flex min-w-0 gap-1.5" @submit.prevent="jumpDir">
          <input ref="dirAddrInput" v-model="dirInput" autocomplete="off" spellcheck="false" enterkeyhint="go"
            aria-label="文件夹路径" placeholder="~"
            class="min-w-0 flex-1 rounded-md border border-[var(--limpet-line)] bg-[var(--limpet-canvas)] px-3 py-1.5 text-sm [font-family:var(--limpet-mono)] focus-visible:outline-[var(--limpet-accent)]"
            @keydown.enter="preventComposingSubmit" />
          <button type="submit" class="control shrink-0 text-sm" :disabled="dirLoading">跳转</button>
        </form>

        <div class="flex min-w-0 items-center gap-1.5">
          <div class="flex min-w-0 flex-1 items-center gap-1 rounded-md border border-[var(--limpet-line)] bg-[var(--limpet-canvas)] px-2 py-1">
            <input v-model="dirFilter" type="search" autocomplete="off" spellcheck="false"
              aria-label="筛选当前目录" placeholder="筛选当前目录…"
              class="min-w-0 flex-1 bg-transparent text-xs focus-visible:outline-none"
              @keydown.enter.prevent="preventComposingSubmit" />
            <button v-if="dirFilter" type="button" class="shrink-0 text-[var(--limpet-muted)] hover:text-[var(--limpet-text)]"
              aria-label="清空筛选" @click="dirFilter = ''">
              <AppIcon name="close" class="size-3.5" />
            </button>
          </div>
          <button type="button" class="control shrink-0 text-xs" :aria-expanded="newDirOpen"
            :disabled="dirLoading || !dirPath" @click="toggleNewDir">
            <AppIcon name="folderPlus" class="size-4" /> 新建
          </button>
          <button type="button" class="control shrink-0 text-xs"
            :class="{ 'text-[var(--limpet-accent)]': showHiddenDirs }"
            :aria-pressed="showHiddenDirs" :disabled="dirLoading" @click="toggleHiddenDirs">
            显示隐藏
          </button>
        </div>

        <form v-if="newDirOpen" class="space-y-1.5 rounded-md border border-[var(--limpet-line)] bg-[var(--limpet-canvas)] p-2"
          @submit.prevent="submitNewDir">
          <div class="flex min-w-0 gap-1.5">
            <input ref="newDirInput" v-model="newDirName" autocomplete="off" spellcheck="false"
              aria-label="新文件夹名称" placeholder="输入新文件夹名称…"
              class="min-w-0 flex-1 rounded-md border border-[var(--limpet-line)] bg-[var(--limpet-surface)] px-2.5 py-1.5 text-sm focus-visible:outline-[var(--limpet-accent)]"
              :disabled="newDirBusy" @keydown.enter="onNewDirEnter" @keydown.escape.stop.prevent="cancelNewDir" />
            <button type="submit" class="control control-primary shrink-0 text-xs" :disabled="newDirBusy">
              {{ newDirBusy ? '创建中…' : '创建' }}
            </button>
            <button type="button" class="control shrink-0 text-xs" :disabled="newDirBusy" @click="cancelNewDir">
              取消
            </button>
          </div>
          <p v-if="newDirError" role="alert" class="wrap-anywhere text-xs text-[var(--limpet-danger)]">{{ newDirError }}</p>
        </form>

        <div class="h-72 max-h-[45dvh] overflow-y-auto overscroll-contain rounded-lg border border-[var(--limpet-line)] bg-[var(--limpet-canvas)] p-1">
          <div v-if="dirError" role="alert" class="space-y-2 p-3 text-xs text-[var(--limpet-danger)]">
            <p class="wrap-anywhere">{{ dirError }}</p>
            <div class="flex gap-1.5">
              <button type="button" class="control text-xs text-[var(--limpet-text)]"
                @click="browseDir(dirInput.trim() || dirPath || '~')">重试</button>
              <button type="button" class="control text-xs text-[var(--limpet-text)]"
                @click="browseDir('~')">回到 ~</button>
            </div>
          </div>
          <p v-else-if="dirLoading && !dirEntries.length && !dirParent" class="p-4 text-center text-xs text-[var(--limpet-muted)]">加载中…</p>
          <ul v-else class="grid min-w-0 gap-0.5">
            <li v-if="dirParent && !dirFilter.trim()" class="min-w-0">
              <button type="button"
                class="dir-entry flex min-h-8 w-full min-w-0 items-center gap-2 rounded px-2.5 py-1 text-left text-sm hover:bg-[var(--limpet-pressed)] active:bg-[var(--limpet-pressed)]"
                :disabled="dirLoading" @click="browseDir(dirParent)">
                <AppIcon name="folder" class="size-4 shrink-0 text-[var(--limpet-muted)]" />
                <span class="min-w-0 flex-1 font-medium [font-family:var(--limpet-mono)]">..</span>
              </button>
            </li>
            <li v-for="entry in filteredDirEntries" :key="entry.path" class="min-w-0">
              <button type="button"
                class="dir-entry group flex min-h-8 w-full min-w-0 items-center gap-2 rounded px-2.5 py-1 text-left text-sm hover:bg-[var(--limpet-pressed)] active:bg-[var(--limpet-pressed)]"
                :disabled="dirLoading" @click="browseDir(entry.path)">
                <AppIcon name="folder" class="size-4 shrink-0 text-[var(--limpet-muted)] group-hover:text-[var(--limpet-accent)]" />
                <bdi class="min-w-0 flex-1 truncate" dir="ltr">{{ entry.name }}</bdi>
                <AppIcon name="right" class="size-3.5 shrink-0 text-[var(--limpet-muted)] opacity-0 transition-opacity group-hover:opacity-100" />
              </button>
            </li>
            <li v-if="!dirEntries.length" class="p-4 text-center text-xs text-[var(--limpet-muted)]">
              当前目录下没有子目录
            </li>
            <li v-else-if="!filteredDirEntries.length" class="p-4 text-center text-xs text-[var(--limpet-muted)]">
              没有匹配的子目录
            </li>
          </ul>
        </div>

        <div class="flex justify-end gap-2 text-sm">
          <button type="button" class="control" @click="closeDirPicker">取消</button>
          <button type="button" class="control control-primary" :disabled="dirLoading" @click="confirmDirPicker">
            选择文件夹
          </button>
        </div>
      </div>

      <form v-else-if="action" class="grid min-w-0 grid-cols-[minmax(0,1fr)] gap-3" @submit.prevent="submitAction">
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
          <div class="flex min-w-0 gap-1.5">
            <input v-model="action.cwd" autocomplete="off" spellcheck="false" aria-label="工作目录" placeholder="~"
              class="min-w-0 flex-1 rounded-md border border-[var(--limpet-line)] bg-[var(--limpet-canvas)] px-3 py-2 text-base focus-visible:outline-[var(--limpet-accent)]"
              @keydown.enter="preventComposingSubmit" />
            <button type="button" class="control shrink-0 text-sm" @click="openDirPicker">
              <AppIcon name="folder" class="size-4" /> 选择
            </button>
          </div>
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
.session-row:hover { background: var(--limpet-hover-soft); }
.session-row-active,
.session-row-active:hover {
  background: var(--limpet-control);
  box-shadow: inset 2px 0 0 var(--limpet-accent);
}
.workspace-heading {
  margin-bottom: 0.125rem;
}
.workspace-group + .workspace-group {
  margin-top: 0.625rem;
  padding-top: 0.625rem;
  border-top: 1px solid var(--limpet-divider);
}
.workspace-toggle {
  color: var(--limpet-heading);
}
.workspace-toggle:hover {
  color: var(--limpet-text);
}
/* 拖动手柄：touch-action 只禁手柄自身，列表其余区域保持原生滚动。
   288px 侧栏宽度预算紧张：手柄与 edit/close 默认收窄（覆盖 .control 的 32px
   最小宽，非触屏环境一律生效），仅粗指针（触屏）放宽为触控热区。 */
.drag-handle {
  touch-action: none;
  min-inline-size: 18px;
  padding-inline: 0;
  opacity: 0.4;
}
.drag-handle:is(:hover, :focus-visible, :active) {
  opacity: 1;
  color: var(--limpet-text);
}
.session-row-action {
  min-inline-size: 26px;
  padding-inline: 0.25rem;
}
@media (pointer: coarse) {
  .drag-handle {
    opacity: 0.75;
    min-inline-size: 36px;
  }
  .session-row-action {
    min-inline-size: 44px;
    padding-inline: 0.5rem;
  }
}
.drag-dragging {
  opacity: 0.45;
  user-select: none;
}
/* 插入位置指示线：用 inset box-shadow 画在目标行边缘，不改变布局高度。 */
.drag-target-before {
  box-shadow: inset 0 2px 0 0 var(--limpet-accent);
}
.drag-target-after {
  box-shadow: inset 0 -2px 0 0 var(--limpet-accent);
}
.order-restore {
  background: var(--limpet-hover-soft);
}
@media (pointer: coarse) {
  .session-entry { min-height: 2.75rem; }
}
.command-badge {
  color: #cbd5e1;
  border-color: rgb(148 163 184 / 30%);
  background: rgb(148 163 184 / 12%);
}
.command-badge:is([data-command="zsh"], [data-command="bash"], [data-command="sh"], [data-command="fish"]) {
  color: #7dd3fc;
  background: rgb(56 189 248 / 14%);
  border-color: rgb(56 189 248 / 34%);
}
.command-badge[data-command="codex"] {
  color: #c4b5fd;
  background: rgb(167 139 250 / 16%);
  border-color: rgb(167 139 250 / 36%);
}
.command-badge[data-command="agy"] {
  color: #fcd34d;
  background: rgb(251 191 36 / 14%);
  border-color: rgb(251 191 36 / 34%);
}
/* 浅色模式：换成较深的饱和文字色配浅色半透明底与边框，避免浅底浅字看不清。 */
:root[data-theme="light"] .command-badge {
  color: #475569;
  background: rgb(100 116 139 / 14%);
  border-color: rgb(100 116 139 / 32%);
}
:root[data-theme="light"] .command-badge:is([data-command="zsh"], [data-command="bash"], [data-command="sh"], [data-command="fish"]) {
  color: #0369a1;
  background: rgb(2 132 199 / 12%);
  border-color: rgb(2 132 199 / 32%);
}
:root[data-theme="light"] .command-badge[data-command="codex"] {
  color: #6d28d9;
  background: rgb(124 58 237 / 12%);
  border-color: rgb(124 58 237 / 30%);
}
:root[data-theme="light"] .command-badge[data-command="agy"] {
  color: #92400e;
  background: rgb(180 83 9 / 12%);
  border-color: rgb(180 83 9 / 30%);
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
  box-shadow: var(--limpet-shadow);
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
  box-shadow: var(--limpet-shadow);
}
@media (pointer: coarse) {
  .dir-entry { min-height: 2.75rem; }
}
.session-dialog {
  margin: auto;
  width: min(29rem, calc(100% - max(1rem, env(safe-area-inset-left)) - max(1rem, env(safe-area-inset-right))));
  max-height: calc(100% - max(1rem, env(safe-area-inset-top)) - max(1rem, env(safe-area-inset-bottom)));
  padding: 1rem;
  overflow: auto;
  border: 1px solid var(--limpet-line);
  border-radius: 0.75rem;
  background: var(--limpet-surface);
  color: var(--limpet-text);
}

.session-dialog:modal { overscroll-behavior: contain; }
.session-dialog::backdrop { background: var(--limpet-backdrop); overflow: hidden; overscroll-behavior: contain; }
</style>
