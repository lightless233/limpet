import { ref } from 'vue'

export interface SessionItem {
  id: string
  name: string
  created: string
  attached: boolean
  cwd?: string
  cmd?: string
}

export function useSessions() {
  const sessions = ref<SessionItem[]>([])
  const loading = ref(true)
  const loadError = ref('')
  const authExpired = ref(false)
  let refreshVersion = 0

  async function request(url: string, init?: RequestInit) {
    const res = await fetch(url, { ...init, redirect: 'manual', cache: 'no-store' })
    if (res.type === 'opaqueredirect') {
      authExpired.value = true
      return
    }
    return res
  }

  async function refresh() {
    if (authExpired.value) return
    const version = ++refreshVersion
    loading.value = true
    loadError.value = ''
    try {
      const res = await request('/api/term')
      if (!res) return
      if (!res.ok) throw new Error('HTTP ' + res.status)
      const list: SessionItem[] = await res.json()
      if (version === refreshVersion) sessions.value = list
    } catch (e) {
      if (version === refreshVersion) loadError.value = e instanceof Error ? e.message : String(e)
    } finally {
      if (version === refreshVersion) loading.value = false
    }
  }

  return { sessions, loading, loadError, authExpired, request, refresh }
}
