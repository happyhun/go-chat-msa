import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        rewrite: (path) => path.replace(/^\/api/, ''),
        cookiePathRewrite: { '/auth': '/api/auth' },
      },
      '/ws-api': {
        target: 'http://localhost:8088',
        rewrite: (path) => path.replace(/^\/ws-api/, ''),
      },
      '/ws': {
        target: 'ws://localhost:8088',
        ws: true,
      },
    },
  },
})
