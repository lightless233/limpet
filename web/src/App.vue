<script setup lang="ts">
// Limpet 根组件：会话列表（首页）与终端视图之间的切换。
// 返回列表不断开后台会话（worker 进程常驻）。
import { ref } from 'vue'
import SessionList from './components/SessionList.vue'
import TerminalView from './components/TerminalView.vue'

type View = { page: 'list' } | { page: 'term'; id: string; name: string }
const view = ref<View>({ page: 'list' })
</script>

<template>
  <SessionList
    v-if="view.page === 'list'"
    @open="(id: string, name: string) => (view = { page: 'term', id, name })"
  />
  <TerminalView
    v-else
    :key="view.id"
    :session-id="view.id"
    :session-name="view.name"
    @back="view = { page: 'list' }"
  />
</template>
