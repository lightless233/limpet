import { ref } from 'vue'

export interface SessionItem {
  id: string
  name: string
  created: string
  attached: boolean
  cwd?: string
  cmd?: string
}

export function workspaceKey(cwd?: string): string {
  return cwd && cwd !== '~' ? cwd : ''
}

export function groupSessions(sessions: SessionItem[]) {
  const groups = new Map<string, { key: string; title: string; items: SessionItem[]; latest: number; parts: string[] }>()
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
    const title = key ? parts.at(-1) ?? key : '主目录'
    groups.set(key, { key, title, items: [session], latest: created, parts })
    if (key) {
      const siblings = byLeaf.get(title)
      if (siblings) siblings.push(parts)
      else byLeaf.set(title, [parts])
    }
  }
  for (const group of groups.values()) {
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
  return [...groups.values()].sort((a, b) => {
    if (!a.key || !b.key) return Number(!a.key) - Number(!b.key)
    return b.latest - a.latest || (a.key < b.key ? -1 : 1)
  })
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
  let settingsVersion = 0
  let appliedSettingsVersion = 0
  let refreshVersion = 0
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

  async function refresh(opts?: { silent?: boolean }) {
    if (authExpired.value) return
    const silent = opts?.silent === true
    if (silent && refreshing.value) return
    const version = ++refreshVersion
    refreshing.value = true
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
    scrollback, loadScrollback, saveScrollback,
  }
}
