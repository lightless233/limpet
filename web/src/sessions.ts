import { ref } from 'vue'

export interface SessionItem {
  id: string
  name: string
  created: string
  attached: boolean
  cwd?: string
  cmd?: string
}

// 「临时工作区」：所有 /tmp 下的会话统一归入的常驻分组，永远沉底。
export const TEMP_WORKSPACE_KEY = '/tmp'
export const TEMP_WORKSPACE_TITLE = '临时工作区'

export function workspaceKey(cwd?: string): string {
  if (cwd === TEMP_WORKSPACE_KEY || cwd?.startsWith(TEMP_WORKSPACE_KEY + '/')) return TEMP_WORKSPACE_KEY
  return cwd && cwd !== '~' ? cwd : ''
}

export interface SessionOrder {
  workspaces: string[]
  sessions: Record<string, string[]>
}

export interface WorkspaceGroup {
  key: string
  title: string
  items: SessionItem[]
  latest: number
  parts: string[]
}

const EMPTY_ORDER: SessionOrder = { workspaces: [], sessions: {} }

export interface GroupOptions {
  includeEmptyTemp?: boolean
}

function byCreatedDesc(a: SessionItem, b: SessionItem): number {
  const diff = Date.parse(b.created) - Date.parse(a.created)
  if (diff) return diff
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0
}

function byLatestDesc(a: WorkspaceGroup, b: WorkspaceGroup): number {
  return b.latest - a.latest || (a.key < b.key ? -1 : 1)
}

// sortGroupItems 组内排序：有手动列表时，列表外的新会话按时间倒序置顶、列表内的按手动顺序
// 排在下面；没有手动列表时整体按 created 倒序（新在上、旧在下，同时间按 id 升序稳定）。
function sortGroupItems(items: SessionItem[], manual?: string[]) {
  if (manual?.length) {
    const rank = new Map(manual.map((id, i) => [id, i]))
    const head = items.filter((item) => !rank.has(item.id)).sort(byCreatedDesc)
    const tail = items.filter((item) => rank.has(item.id)).sort((a, b) => rank.get(a.id)! - rank.get(b.id)!)
    items.length = 0
    items.push(...head, ...tail)
    return
  }
  items.sort(byCreatedDesc)
}

// sortGroups 组间排序：「临时工作区」永远沉底；其余工作区在无手动列表时按组内最新会话的
// created 倒序（同时间按 key 升序稳定），有手动列表时未记录的新工作区置顶（按时间倒序）、
// 已记录的按手动顺序排在下面。
function sortGroups(groups: WorkspaceGroup[], manual?: string[]) {
  const tempIndex = groups.findIndex((group) => group.key === TEMP_WORKSPACE_KEY)
  const temp = tempIndex >= 0 ? groups.splice(tempIndex, 1)[0] : undefined
  if (manual?.length) {
    const rank = new Map(manual.map((key, i) => [key, i]))
    const head = groups.filter((group) => !rank.has(group.key)).sort(byLatestDesc)
    const tail = groups.filter((group) => rank.has(group.key)).sort((a, b) => rank.get(a.key)! - rank.get(b.key)!)
    groups.length = 0
    groups.push(...head, ...tail)
  } else {
    groups.sort(byLatestDesc)
  }
  if (temp) groups.push(temp)
}

export function groupSessions(sessions: SessionItem[], order: SessionOrder = EMPTY_ORDER, opts: GroupOptions = {}) {
  const groups = new Map<string, WorkspaceGroup>()
  const byLeaf = new Map<string, string[][]>()
  for (const session of sessions) {
    const key = workspaceKey(session.cwd)
    const created = Date.parse(session.created)
    const group = groups.get(key)
    if (group) {
      group.items.push(session)
      group.latest = Math.max(group.latest, created)
      continue
    }
    const parts = key.split('/').filter(Boolean)
    const title = key === TEMP_WORKSPACE_KEY ? TEMP_WORKSPACE_TITLE : key ? parts.at(-1) ?? key : '主目录'
    groups.set(key, { key, title, items: [session], latest: created, parts })
    if (key && key !== TEMP_WORKSPACE_KEY) {
      const siblings = byLeaf.get(title)
      if (siblings) siblings.push(parts)
      else byLeaf.set(title, [parts])
    }
  }
  for (const group of groups.values()) {
    if (group.key === TEMP_WORKSPACE_KEY) continue
    const siblings = byLeaf.get(group.title)
    if (!group.key || !siblings || siblings.length === 1) continue
    group.title = group.key
    for (let depth = 2; depth <= group.parts.length; depth++) {
      const suffix = group.parts.slice(-depth).join('/')
      if (siblings.every((parts) => parts === group.parts || parts.slice(-depth).join('/') !== suffix)) {
        group.title = suffix
        break
      }
    }
  }
  const list = [...groups.values()]
  for (const group of list) sortGroupItems(group.items, order.sessions?.[group.key])
  if (opts.includeEmptyTemp && !groups.has(TEMP_WORKSPACE_KEY)) {
    list.push({ key: TEMP_WORKSPACE_KEY, title: TEMP_WORKSPACE_TITLE, items: [], latest: 0, parts: ['tmp'] })
  }
  sortGroups(list, order.workspaces)
  return list
}

const POLL_INTERVAL_MS = 5000
export const MAX_SCROLLBACK = 1_000_000

export interface ScrollbackResult {
  scrollback: number
  applied: number
  unsupported: number
  failed: number
}

export function useSessions() {
  const sessions = ref<SessionItem[]>([])
  const loading = ref(true)
  const refreshing = ref(false)
  const loadError = ref('')
  const authExpired = ref(false)
  const scrollback = ref(10_000)
  const order = ref<SessionOrder>({ workspaces: [], sessions: {} })
  let settingsVersion = 0
  let appliedSettingsVersion = 0
  let refreshVersion = 0
  let orderVersion = 0
  let appliedOrderVersion = 0
  let savingOrder = false
  let scheduleNextPoll: (() => void) | undefined

  async function request(url: string, init?: RequestInit) {
    const res = await fetch(url, { ...init, redirect: 'manual', cache: 'no-store' })
    if (res.type === 'opaqueredirect') {
      authExpired.value = true
      return
    }
    return res
  }

  async function loadScrollback() {
    if (authExpired.value) return scrollback.value
    const version = ++settingsVersion
    try {
      const res = await request('/api/settings', { signal: AbortSignal.timeout(2000) })
      if (res?.ok) {
        const data: Pick<ScrollbackResult, 'scrollback'> = await res.json()
        if (version > appliedSettingsVersion) {
          scrollback.value = data.scrollback
          appliedSettingsVersion = version
        }
      }
    } catch {
      // A failed read leaves the last successful value (or the default) usable.
    }
    return scrollback.value
  }

  async function saveScrollback(lines: number) {
    const res = await request('/api/settings', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ scrollback: lines }),
    })
    if (!res) throw new Error('登录已失效')
    if (!res.ok) {
      const data = await res.json().catch(() => ({}))
      throw new Error(data.error ?? 'HTTP ' + res.status)
    }
    const result: ScrollbackResult = await res.json()
    appliedSettingsVersion = ++settingsVersion
    scrollback.value = result.scrollback
    return result
  }

  // normalizeOrder 只保留合法的手动排序：过滤 workspaces 里的「临时工作区」与重复项，
  // 过滤每组会话 ID 的空串与重复项，清掉空数组 key（与服务端 order.json 的规范一致）。
  function normalizeOrder(data: SessionOrder): SessionOrder {
    const workspaces: string[] = []
    for (const key of Array.isArray(data?.workspaces) ? data.workspaces : []) {
      if (typeof key === 'string' && key !== TEMP_WORKSPACE_KEY && !workspaces.includes(key)) {
        workspaces.push(key)
      }
    }
    const sessions: Record<string, string[]> = {}
    for (const [key, ids] of Object.entries(data?.sessions ?? {})) {
      if (!Array.isArray(ids)) continue
      const cleaned: string[] = []
      for (const id of ids) {
        if (typeof id === 'string' && id && !cleaned.includes(id)) cleaned.push(id)
      }
      if (cleaned.length) sessions[key] = cleaned
    }
    return { workspaces, sessions }
  }

  // loadOrder 拉取服务端手动排序（手机/电脑自动同步）。递增版本号保证：
  // 刚刚 PUT 的本地结果不会被还在路上的旧 GET 响应覆盖。
  async function loadOrder() {
    if (authExpired.value || savingOrder) return
    const version = ++orderVersion
    try {
      const res = await request('/api/order', { signal: AbortSignal.timeout(2000) })
      if (!res?.ok) return
      const data: SessionOrder = await res.json()
      if (version > appliedOrderVersion) {
        order.value = normalizeOrder(data)
        appliedOrderVersion = version
      }
    } catch {
      // 排序读取失败（多为瞬态网络问题）保留当前值，下一轮轮询会再试。
    }
  }

  // saveOrder 乐观更新本地排序并 PUT 到服务端；失败回滚并抛错供界面提示。
  async function saveOrder(next: SessionOrder) {
    const normalized = normalizeOrder(next)
    const previous = order.value
    savingOrder = true
    order.value = normalized
    appliedOrderVersion = ++orderVersion
    try {
      const res = await request('/api/order', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(normalized),
      })
      if (!res) throw new Error('登录已失效')
      if (!res.ok) {
        const data = await res.json().catch(() => ({}))
        throw new Error(data.error ?? 'HTTP ' + res.status)
      }
      const saved: SessionOrder = await res.json()
      order.value = normalizeOrder(saved)
    } catch (e) {
      order.value = previous
      appliedOrderVersion = ++orderVersion
      throw e
    } finally {
      savingOrder = false
    }
  }

  async function resetOrder() {
    await saveOrder({ workspaces: [], sessions: {} })
  }

  async function refresh(opts?: { silent?: boolean }) {
    if (authExpired.value) return
    const silent = opts?.silent === true
    if (silent && refreshing.value) return
    const version = ++refreshVersion
    refreshing.value = true
    void loadOrder()
    if (!silent || !sessions.value.length) loading.value = true
    if (!silent) loadError.value = ''
    try {
      const res = await request('/api/term')
      if (!res) return
      if (!res.ok) throw new Error('HTTP ' + res.status)
      const list: SessionItem[] = await res.json()
      if (version === refreshVersion) {
        sessions.value = list
        loadError.value = ''
      }
    } catch (e) {
      if (version === refreshVersion && (!silent || !sessions.value.length)) {
        loadError.value = e instanceof Error ? e.message : String(e)
      }
    } finally {
      if (version === refreshVersion) {
        loading.value = false
        refreshing.value = false
        scheduleNextPoll?.()
      }
    }
  }

  function startAutoRefresh(shouldRefreshOnOnline: () => boolean) {
    let stopped = false
    let timer = 0

    const clearPoll = () => {
      clearTimeout(timer)
      timer = 0
    }

    const schedule = () => {
      clearPoll()
      if (stopped || authExpired.value || document.visibilityState !== 'visible') return
      timer = window.setTimeout(() => {
        timer = 0
        void refresh({ silent: true })
      }, POLL_INTERVAL_MS)
    }

    scheduleNextPoll = schedule

    const onVisibility = () => {
      if (document.visibilityState === 'visible') {
        clearPoll()
        void refresh({ silent: true })
      } else {
        clearPoll()
      }
    }
    const onFocus = () => {
      if (document.visibilityState === 'visible') void refresh({ silent: true })
    }
    const onOnline = () => {
      if (shouldRefreshOnOnline()) void refresh({ silent: true })
    }

    document.addEventListener('visibilitychange', onVisibility)
    window.addEventListener('focus', onFocus)
    window.addEventListener('online', onOnline)
    schedule()

    return () => {
      stopped = true
      scheduleNextPoll = undefined
      clearPoll()
      document.removeEventListener('visibilitychange', onVisibility)
      window.removeEventListener('focus', onFocus)
      window.removeEventListener('online', onOnline)
    }
  }

  return {
    sessions, loading, refreshing, loadError, authExpired, request, refresh, startAutoRefresh,
    scrollback, loadScrollback, saveScrollback, order, saveOrder, resetOrder,
  }
}
