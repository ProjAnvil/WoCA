import { defineStore } from 'pinia'
import { ref } from 'vue'
import { startBtwRun, streamRun } from '../api/client'
import { toErrorMessage } from '../lib/errors'

interface BtwEntry {
  role: 'user' | 'assistant'
  content: string
}

// "/btw" side channel: questions answered with the same agent and session
// history but NEVER persisted to the transcript (the server-side run skips
// s.sessions.save). The panel is ephemeral — close clears it.
export const useBtwStore = defineStore('btw', () => {
  const open = ref(false)
  const entries = ref<BtwEntry[]>([])
  const running = ref(false)
  const error = ref('')

  function show() {
    open.value = true
  }
  function hide() {
    open.value = false
  }
  function clear() {
    entries.value = []
    error.value = ''
  }

  async function ask(prompt: string, sessionId: string, workspace?: string) {
    const text = prompt.trim()
    if (!text || running.value || !sessionId) return
    open.value = true
    entries.value = [...entries.value, { role: 'user', content: text }]
    running.value = true
    error.value = ''
    try {
      const { run_id } = await startBtwRun(sessionId, text, workspace)
      await streamRun(run_id, {
        onDelta: (t) => {
          const last = entries.value[entries.value.length - 1]
          if (last?.role === 'assistant') last.content += t
          else entries.value = [...entries.value, { role: 'assistant', content: t }]
        },
        onToolCall: () => {},
        onToolResult: () => {},
        onInterrupt: () => {},
        onDone: () => {
          running.value = false
        },
        onError: (msg) => {
          error.value = msg
          running.value = false
        },
      })
    } catch (e) {
      error.value = toErrorMessage(e)
      running.value = false
    }
  }

  return { open, entries, running, error, show, hide, clear, ask }
})
