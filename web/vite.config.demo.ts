import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// Standalone demo entry. Run `npm run dev:demo` (http://localhost:5174) to
// preview the UI with mock data, independent of the Go backend.
export default defineConfig({
  root: 'demo',
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    port: 5174,
  },
  build: {
    outDir: '../dist-demo',
    emptyOutDir: true,
  },
})
