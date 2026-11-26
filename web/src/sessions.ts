import { ref } from 'vue'

export interface SessionItem {
  id: string
  name: string
  created: string
  attached: boolean
  cwd?: string
  cmd?: string
}

const POLL_INTERVAL_MS = 5000

export function useSessions() {
  const sessions = ref<SessionItem[]>([])
  const loading = ref(true)
  const refreshing = ref(false)
  const loadError = ref('')
  const authExpired = ref(false)
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

  return { sessions, loading, refreshing, loadError, authExpired, request, refresh, startAutoRefresh }
}
