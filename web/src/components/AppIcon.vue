<script setup lang="ts">
const paths = {
  add: 'M12 5v14M5 12h14',
  edit: 'M12 20h8M4 20l4-.8 11-11a2.1 2.1 0 0 0-3-3l-11 11L4 20Zm10-13 3 3',
  close: 'm6 6 12 12M18 6 6 18',
  terminal: 'm5 7 5 5-5 5m8 0h6',
  back: 'm12 6-6 6 6 6M6 12h13',
  arrowUp: 'M12 19V5m-6 6 6-6 6 6',
  down: 'm6 9 6 6 6-6',
  lock: 'M8 11V8a4 4 0 1 1 8 0v3M7 11h10a2 2 0 0 1 2 2v5a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2v-5a2 2 0 0 1 2-2Z',
  more: 'M5 12h.01M12 12h.01M19 12h.01',
  minus: 'M5 12h14',
  bottom: 'm6 12 6 6 6-6M12 18V4M5 21h14',
  composer: 'M4 5h16a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1H9l-5 3v-3a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1Zm3 4h10m-10 4h7',
  refresh: 'M20 12a8 8 0 1 1-2.3-5.6M20 4v5h-5',
  folder: 'M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7Z',
  folderOpen: 'M3 7a2 2 0 0 1 2-2h4l2 2h7a2 2 0 0 1 2 2v1M3 7v10a2 2 0 0 0 2 2h13.2a2 2 0 0 0 1.9-1.4l1.5-5A1.5 1.5 0 0 0 20.2 10H6.8a2 2 0 0 0-1.9 1.4L3 17',
}

defineProps<{ name: keyof typeof paths }>()
</script>

<template>
  <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor"
    stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path :d="paths[name]" />
  </svg>
</template>
