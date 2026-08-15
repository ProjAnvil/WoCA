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

  async function load() {
    if (loaded.value) return
    try {
      const res = await getCompletions()
      skills.value = res.skills ?? []
      agents.value = res.agents ?? []
    } catch {
      // Autocomplete is optional; an empty list just hides the popup.
    } finally {
      loaded.value = true
    }
  }

  function seed(seedSkills: CompletionEntry[], seedAgents: CompletionEntry[]) {
    skills.value = seedSkills
    agents.value = seedAgents
    loaded.value = true
  }

  return { skills, agents, loaded, load, seed }
})
