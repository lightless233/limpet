<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, useTemplateRef } from 'vue'
import AppIcon from './components/AppIcon.vue'
import SessionList from './components/SessionList.vue'
import TerminalView from './components/TerminalView.vue'
import { useSessions } from './sessions'

const store = useSessions()
const list = useTemplateRef('list')
const activeId = ref<string>()
const initialName = ref('')
const activeName = computed(() => store.sessions.value.find((s) => s.id === activeId.value)?.name ?? initialName.value)
let stopAutoRefresh: (() => void) | undefined

function open(id: string, name: string) {
  activeId.value = id
  initialName.value = name
}

function back() {
  activeId.value = undefined
  void store.refresh()
}

onMounted(() => {
  void store.refresh()
  void store.loadScrollback()
  stopAutoRefresh = store.startAutoRefresh(() => !activeId.value)
})

onBeforeUnmount(() => {
  stopAutoRefresh?.()
})
</script>

<template>
  <div class="grid h-dvh min-w-0 grid-cols-[minmax(0,1fr)] lg:grid-cols-[18rem_minmax(0,1fr)]">
    <SessionList
      ref="list"
      :class="activeId ? 'hidden lg:flex' : 'flex'"
      :store="store"
      :active-id="activeId"
      @open="open"
    />
    <TerminalView
      v-if="activeId"
      :key="activeId"
      :session-id="activeId"
      :session-name="activeName"
      :sessions="store.sessions.value"
      :sessions-loading="store.loading.value"
      :sessions-error="store.loadError.value"
      :auth-expired="store.authExpired.value"
      :scrollback="store.scrollback.value"
      :load-scrollback="store.loadScrollback"
      :save-scrollback="store.saveScrollback"
      @refresh-sessions="store.refresh"
      @session-auth-expired="store.authExpired.value = true"
      @open-session="open"
      @back="back"
    />
    <main v-else class="hidden min-h-0 min-w-0 place-items-center lg:grid">
      <div class="grid justify-items-center gap-4 text-sm text-[var(--limpet-muted)]">
        <AppIcon name="terminal" />
        <p>选择会话以打开终端</p>
        <button v-if="!store.authExpired.value" class="control" @click="list?.openCreate()">
          <AppIcon name="add" /> 新建会话
        </button>
      </div>
    </main>
  </div>
</template>
