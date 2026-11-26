<script setup lang="ts">
const paths = {
  add: 'M12 5v14M5 12h14',
  edit: 'M12 20h8M4 20l4-.8 11-11a2.1 2.1 0 0 0-3-3l-11 11L4 20Zm10-13 3 3',
  close: 'm6 6 12 12M18 6 6 18',
  terminal: 'm5 7 5 5-5 5m8 0h6',
}

defineProps<{ name: keyof typeof paths }>()
</script>

<template>
  <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor"
    stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path :d="paths[name]" />
  </svg>
</template>
