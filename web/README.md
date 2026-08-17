# woca web

The web frontend of woca (Vue 3 + TypeScript + Vite). The production build is
embedded into the Go binary via `go:embed web/dist`, giving a single-binary
distribution.

## Tech stack

- Vue 3 (`<script setup lang="ts">`)
- Pinia (session / workspace / settings state)
- @vueuse/core (`useDark` theme switching)
- lucide-vue-next (icons)
- marked + dompurify (Markdown rendering + XSS sanitization)
- shiki (code highlighting for file previews; fine-grained lazy loading)
- @microsoft/fetch-event-source (SSE streaming client)

The chat input supports image attachments (paperclip upload or
Cmd+V / Ctrl+V screenshot paste, sent with the message as multimodal content
blocks) plus `/`-prefix completion for skills and `@`-prefix completion for
subagents (arrow keys to select, Enter/Tab to confirm, Esc to cancel; the
highlighted item shows its description).

## Development

```bash
npm install
npm run dev        # http://localhost:5173 (/api proxies to 127.0.0.1:8899)
```

## Build

```bash
npm run build      # vue-tsc type-check + vite build, output to dist/
```

## Standalone demo

A self-contained demo that runs without the Go backend and previews the whole
UI with rich mock data (sessions, streaming messages, tool cards, Markdown
previews, approval modals, themes):

```bash
npm run dev:demo       # http://localhost:5174
npm run build:demo     # builds into dist-demo/
```

Demo data lives in `demo/data.ts`; the demo page has buttons at the top that
trigger the three approval modals (permission / question / plan approval). The
production app does **not** include any mock data.

## Layout

```
src/
├─ main.ts                 # entry: createApp + pinia
├─ style.css               # design tokens (light/dark)
├─ types.ts                # session / message / workspace-file / transcript types
├─ lib/markdown.ts         # marked + dompurify rendering
├─ lib/highlight.ts        # shiki wrapper (extension → language mapping)
├─ lib/highlighter-engine.ts # shiki grammar bundles (lazy chunk)
├─ lib/files.ts            # client-side previewability checks
├─ api/client.ts           # REST + SSE client (fetch-event-source)
├─ stores/
│  ├─ settings.ts          # theme + settings panel visibility + preferences
│  ├─ sessions.ts          # sessions + messages + run state (incl. demo mode)
│  ├─ completions.ts       # / command and @ subagent completion lists
│  └─ workspace.ts         # workspace file tree + preview
└─ components/
   ├─ Sidebar.vue          # left: sessions grouped by workspace + settings entry
   ├─ SessionList.vue      # collapsible workspace-grouped session list
   ├─ Avatar.vue           # chat bubble avatars (woca bot / user)
   ├─ WorkspaceSidebar.vue # right: workspace file browser sidebar (collapsible)
   ├─ WorkspaceTree.vue    # recursive file tree
   ├─ FilePreview.vue      # file preview (markdown / shiki highlighting)
   ├─ TreeNode.vue
   ├─ SettingsPanel.vue    # card-style settings (theme/preferences/model)
   ├─ ChatPanel.vue        # chat pane + attachment bar + completion popovers
   ├─ ChatItemView.vue     # message bubbles (user right / woca left) + tool cards
   └─ InterruptModal.vue   # permission / question / plan approval modals
demo/
├─ index.html              # demo entry
├─ main.ts
├─ App.vue                 # demo layout + approval trigger buttons
└─ data.ts                 # rich mock data
```

## Theming

Themes are implemented with CSS variables and a `.dark` class on `<html>`.
`useDark` persists the choice to `localStorage["woca-theme"]` and follows the
system preference; an inline snippet in `index.html` applies the theme before
first paint to avoid flicker.
