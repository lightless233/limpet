import tailwindcss from '@tailwindcss/vite'
import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [vue(), tailwindcss()],
  server: {
    // 开发阶段固定 4041，/api 代理到 Go 服务 (4040)，
    // ws: true 让 WebSocket 也走同一个代理。
    port: 4041,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:4040',
        ws: true,
      },
    },
  },
})
