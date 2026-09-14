import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5174,
    proxy: {
      '/api': {
        target: process.env.API_PROXY_TARGET || 'http://127.0.0.1:8090',
        // 保持 Host 原样(localhost:5174),后端 WS 同源校验靠 Origin 与 Host 一致放行;
        // 若开启 changeOrigin 会重写 Host 导致开发模式 WebSocket 握手被拒
        changeOrigin: false,
        ws: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    chunkSizeWarningLimit: 4096,
  },
})
