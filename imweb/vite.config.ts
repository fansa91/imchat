import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 开发时把接口代理到服务端：IM_SERVER=http://127.0.0.1:8092 npm run dev
const target = process.env.IM_SERVER || 'http://127.0.0.1:8092'
const proxy = {
  '/api': { target, changeOrigin: true },
  '/ws': { target, ws: true, changeOrigin: true },
}

export default defineConfig({
  plugins: [react()],
  server: { proxy, host: true },
  preview: { proxy, host: true },
  build: { target: 'es2022', chunkSizeWarningLimit: 1000 },
})
