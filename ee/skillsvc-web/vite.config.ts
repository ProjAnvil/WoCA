import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  // The console is served by the woca-ops facade under /skillsvc/ — asset
  // URLs must be rooted there; API calls stay absolute (/api/skills) and
  // ride the facade's /api channel.
  base: '/skillsvc/',
  plugins: [vue(), tailwindcss()],
  build: { outDir: 'dist' },
})
