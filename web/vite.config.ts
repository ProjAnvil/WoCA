import { defineConfig, type Plugin } from 'vite'
import vue from '@vitejs/plugin-vue'

// Vite emits crossorigin attributes on the built module script and stylesheet
// link. The Wails WKURLSchemeHandler serves the native shell from an opaque
// wails:// origin without CORS headers, so WebKit silently drops any
// crossorigin-tagged stylesheet there (browsers over HTTP are same-origin and
// unaffected). Strip the attribute for the embedded SPA in both editions.
function stripCrossorigin(): Plugin {
  return {
    name: 'strip-crossorigin-for-embedded-webview',
    apply: 'build',
    transformIndexHtml(html) {
      return html.replace(/ crossorigin(?=[ >])/g, '')
    },
  }
}

// Static SPA that the Go binary embeds via `go:embed web/dist`.
// The `/api` proxy targets the woca HTTP server during development so the
// frontend can call it without CORS; production serves both from one origin.
export default defineConfig({
  plugins: [vue(), stripCrossorigin()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        // Native wails-dev HMR mode points this at the embedded bridge's
        // fixed dev port (set via frontend:dev:start in wails.json); the
        // personal `woca serve` workflow keeps the historical default.
        target: process.env.VITE_PROXY_TARGET || 'http://127.0.0.1:8899',
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
