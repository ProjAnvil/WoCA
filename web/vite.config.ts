import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// Static SPA that the Go binary embeds via `go:embed web/dist`.
// The `/api` proxy targets the woca HTTP server during development so the
// frontend can call it without CORS; production serves both from one origin.
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8899',
        changeOrigin: true,
      },
    },
  },
  build: {
    target: 'es2022',
    outDir: 'dist',
    sourcemap: false,
  },
})
