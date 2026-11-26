<script setup lang="ts">
import { computed, onMounted, ref, useTemplateRef } from 'vue'
import AppIcon from './components/AppIcon.vue'
import SessionList from './components/SessionList.vue'
import TerminalView from './components/TerminalView.vue'
import { useSessions } from './sessions'

const store = useSessions()
const list = useTemplateRef('list')
const activeId = ref<string>()
const initialName = ref('')
const activeName = computed(() => store.sessions.value.find((s) => s.id === activeId.value)?.name ?? initialName.value)

function open(id: string, name: string) {
  activeId.value = id
  initialName.value = name
}

function back() {
  activeId.value = undefined
  void store.refresh()
}

onMounted(store.refresh)
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
