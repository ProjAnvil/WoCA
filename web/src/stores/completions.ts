import { defineStore } from 'pinia'
import { ref } from 'vue'
import type { CompletionEntry } from '../types'
import { getCompletions } from '../api/client'

// Skills ("/" prefix) and subagents ("@" prefix) for the chat input's
// autocomplete popup. Fetched lazily on first use; the standalone demo seeds
// mock entries instead.
export const useCompletionsStore = defineStore('completions', () => {
  const skills = ref<CompletionEntry[]>([])
  const agents = ref<CompletionEntry[]>([])
  const loaded = ref(false)
  // The workspace the lists were fetched for ('' = the server's startup
  // directory); switching to a session in another workspace refetches so
  // the popup matches what the model can invoke there.
  const root = ref('')

  async function load(workspace?: string) {
    const ws = workspace ?? root.value
    if (loaded.value && ws === root.value) return
    try {
      const res = await getCompletions(ws || undefined)
      skills.value = res.skills ?? []
      agents.value = res.agents ?? []
      root.value = ws
      loaded.value = true
    } catch {
      // Autocomplete is optional; an empty list just hides the popup.
    }
  }

  function seed(seedSkills: CompletionEntry[], seedAgents: CompletionEntry[]) {
    skills.value = seedSkills
    agents.value = seedAgents
    loaded.value = true
  }

  return { skills, agents, loaded, root, load, seed }
})
