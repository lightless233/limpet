<script setup lang="ts">
// Limpet 根组件：会话列表（首页）与终端视图之间的切换。
// 返回列表不断开后台会话（tmux 常驻）。
import { ref } from 'vue'
import SessionList from './components/SessionList.vue'
import TerminalView from './components/TerminalView.vue'

type View = { page: 'list' } | { page: 'term'; id: string }
const view = ref<View>({ page: 'list' })
</script>

<template>
  <SessionList
    v-if="view.page === 'list'"
    @open="(id: string) => (view = { page: 'term', id })"
  />
  <TerminalView
    v-else
    :key="view.id"
    :session-id="view.id"
    @back="view = { page: 'list' }"
  />
</template>
