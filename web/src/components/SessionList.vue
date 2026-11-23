<script setup lang="ts">
// 会话列表（移动端即首页）：新建（可命名）/ 接入 / 重命名 / 关闭。
// 数据实时来自后端（worker 进程是事实来源），增删改后刷新。
// 显示一律用 name（显示名）；id（uuid）只用于寻址，不出现在界面上。
import { nextTick, onMounted, ref, watch } from 'vue'

interface SessionItem {
  id: string
  name: string
  created: string
  attached: boolean
}

const emit = defineEmits<{ open: [id: string, name: string] }>()

const sessions = ref<SessionItem[]>([])
const loading = ref(true)
const loadError = ref('')
const busy = ref(false)

// 底部弹层：null = 关闭
type Sheet = null | { mode: 'create'; name: string } | { mode: 'rename'; id: string; name: string }
const sheet = ref<Sheet>(null)
const sheetError = ref('')
const nameInput = ref<HTMLInputElement | null>(null)

watch(sheet, (v) => {
  if (v) nextTick(() => nameInput.value?.focus())
})

async function refresh() {
  loading.value = true
  loadError.value = ''
  try {
    const res = await fetch('/api/term')
    if (!res.ok) throw new Error(`HTTP ${res.status}`)
    sessions.value = await res.json()
  } catch (e) {
    loadError.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

onMounted(refresh)

async function submitSheet() {
  const s = sheet.value
  if (!s || busy.value) return
  busy.value = true
  sheetError.value = ''
  try {
    const url = s.mode === 'create' ? '/api/term' : `/api/term/${encodeURIComponent(s.id)}/rename`
    const body = s.mode === 'create' ? { name: s.name.trim() } : { name: s.name.trim() }
    const res = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
    if (!res.ok) {
      const data = await res.json().catch(() => ({}))
      throw new Error(data.error ?? `HTTP ${res.status}`)
    }
    sheet.value = null
    await refresh()
  } catch (e) {
    sheetError.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}

const closeError = ref('')

async function closeSession(s: SessionItem) {
  if (!confirm(`关闭会话「${s.name}」？其中的 Shell 将被终止。`)) return
  closeError.value = ''
  try {
    const res = await fetch(`/api/term/${encodeURIComponent(s.id)}/close`, { method: 'POST' })
    if (!res.ok) {
      const data = await res.json().catch(() => ({}))
      throw new Error(data.error ?? `HTTP ${res.status}`)
    }
    await refresh()
  } catch (e) {
    closeError.value = `关闭失败: ${e instanceof Error ? e.message : String(e)}`
  }
}

function fmtTime(iso: string) {
  const d = new Date(iso)
  const now = new Date()
  const hm = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
  return d.toDateString() === now.toDateString()
    ? `今天 ${hm}`
    : `${d.getMonth() + 1}/${d.getDate()} ${hm}`
}
</script>

<template>
  <div class="flex h-dvh flex-col bg-neutral-950 text-neutral-200 pt-[env(safe-area-inset-top)]">
    <header class="flex items-center justify-between px-4 pb-2 pt-3">
      <h1 class="text-xl font-bold tracking-tight">Limpet</h1>
      <button
        class="rounded-md bg-neutral-800 px-3 py-2 text-sm active:scale-95 hover:bg-neutral-700"
        @click="sheet = { mode: 'create', name: '' }; sheetError = ''"
      >
        ＋ 新建
      </button>
    </header>

    <main class="min-h-0 flex-1 overflow-y-auto px-3 pb-[max(env(safe-area-inset-bottom),0.75rem)]">
      <p v-if="closeError" class="px-1 py-2 text-sm text-red-400">{{ closeError }}</p>
      <p v-if="loadError" class="px-1 py-2 text-sm text-red-400">{{ loadError }}</p>
      <p v-else-if="loading && !sessions.length" class="px-1 py-2 text-sm text-neutral-500">加载中…</p>
      <p v-else-if="!sessions.length" class="px-1 py-6 text-center text-sm text-neutral-500">
        还没有会话<br />点右上角「新建」开始
      </p>

      <ul class="space-y-2">
        <li v-for="s in sessions" :key="s.id">
          <div class="flex items-center gap-1 rounded-lg border border-neutral-800 bg-neutral-900/60 p-1">
            <button
              class="flex min-w-0 flex-1 items-center gap-3 rounded-md px-3 py-3 text-left active:bg-neutral-800"
              @click="emit('open', s.id, s.name)"
            >
              <span
                class="h-2 w-2 shrink-0 rounded-full"
                :class="s.attached ? 'bg-emerald-400' : 'bg-neutral-600'"
                :title="s.attached ? '使用中' : '空闲'"
              ></span>
              <span class="min-w-0">
                <span class="block truncate font-medium">{{ s.name }}</span>
                <span class="block text-xs text-neutral-500">{{ fmtTime(s.created) }}</span>
              </span>
            </button>
            <button
              class="h-11 w-11 shrink-0 rounded-md text-neutral-400 active:bg-neutral-800 hover:text-neutral-200"
              title="重命名"
              @click.stop="sheet = { mode: 'rename', id: s.id, name: s.name }; sheetError = ''"
            >
              ✎
            </button>
            <button
              class="h-11 w-11 shrink-0 rounded-md text-neutral-400 active:bg-neutral-800 hover:text-red-400"
              title="关闭"
              @click.stop="closeSession(s)"
            >
              ✕
            </button>
          </div>
        </li>
      </ul>
    </main>

    <!-- 底部弹层：新建 / 重命名 -->
    <div
      v-if="sheet"
      class="fixed inset-0 z-10 flex flex-col justify-end bg-black/60"
      @click.self="sheet = null"
    >
      <div class="rounded-t-2xl bg-neutral-900 p-4 pb-[max(env(safe-area-inset-bottom),1rem)]">
        <h2 class="mb-3 text-sm font-semibold text-neutral-300">
          {{ sheet.mode === 'create' ? '新建会话' : '重命名会话' }}
        </h2>
        <input
          ref="nameInput"
          v-model="sheet.name"
          class="w-full rounded-md border border-neutral-700 bg-neutral-950 px-3 py-2.5 text-base outline-none focus:border-neutral-500"
          placeholder="会话名（留空自动生成）"
          enterkeyhint="done"
          autocomplete="off"
          spellcheck="false"
          @keyup.enter="submitSheet"
        />
        <p v-if="sheetError" class="mt-2 text-sm text-red-400">{{ sheetError }}</p>
        <div class="mt-4 flex gap-2">
          <button
            class="flex-1 rounded-md bg-neutral-800 py-2.5 text-sm active:scale-95"
            @click="sheet = null"
          >
            取消
          </button>
          <button
            class="flex-1 rounded-md bg-emerald-600 py-2.5 text-sm font-medium text-white active:scale-95 disabled:opacity-50"
            :disabled="busy"
            @click="submitSheet"
          >
            确定
          </button>
        </div>
      </div>
    </div>
  </div>
</template>
