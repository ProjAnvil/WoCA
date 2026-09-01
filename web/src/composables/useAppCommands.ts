import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { MODE_LABELS, useAgentStore } from '../stores/agent'
import { useMemoryStore } from '../stores/memory'
import { useSessionStore } from '../stores/sessions'
import { useSettingsStore } from '../stores/settings'
import { useToastStore } from '../stores/toast'
import { useWorkspaceStore } from '../stores/workspace'
import { useBtwStore } from '../stores/btw'
import type { PermissionMode } from '../types'

export type PaletteCommandGroup = 'sessions' | 'panels' | 'mode' | 'appearance' | 'actions'

export interface PaletteCommand {
  id: string
  group: PaletteCommandGroup
  label: string
  hint?: string
  keywords?: string
  run: () => void
}

/**
 * The command registry consumed by WCommandPalette. Assembled from the live
 * stores so entries reflect the current world (busy run → interrupt shows,
 * assistant reply exists → copy shows). This is the single source of truth
 * the ⌘K palette and any future native menu bar both drive.
 */
export function useAppCommands() {
  const { t } = useI18n()
  const sessions = useSessionStore()
  const workspace = useWorkspaceStore()
  const memory = useMemoryStore()
  const settings = useSettingsStore()
  const agent = useAgentStore()
  const toast = useToastStore()
  const btw = useBtwStore()

  return computed<PaletteCommand[]>(() => {
    const cmds: PaletteCommand[] = []

    // Session jumps: most recent first, capped so the palette stays snappy
    // on installs with long histories.
    const recent = (sessions.workspaceGroups ?? [])
      .flatMap((g) => g.sessions.map((s) => ({ ...s, wsName: g.name })))
      .sort((a, b) => (b.updatedAt || '').localeCompare(a.updatedAt || ''))
      .slice(0, 20)
    for (const s of recent) {
      cmds.push({
        id: `session:${s.id}`,
        group: 'sessions',
        label: s.title || t('sidebar.newSession'),
        hint: s.wsName,
        keywords: 'session jump open',
        run: () => sessions.setActive(s.id),
      })
    }
    const ws = sessions.activeWorkspace
    cmds.push({
      id: 'session:new',
      group: 'sessions',
      label: t('palette.newSession'),
      keywords: 'new session create',
      run: () => sessions.createSession({ path: ws?.path, name: ws?.name }),
    })

    cmds.push({
      id: 'panel:workspace',
      group: 'panels',
      label: t('palette.toggleWorkspace'),
      keywords: 'sidebar right files',
      run: () => workspace.togglePanel(),
    })
    cmds.push({
      id: 'panel:goal',
      group: 'panels',
      label: t('palette.toggleGoal'),
      run: () => {
        sessions.goalPanelOpen = true
      },
    })
    cmds.push({
      id: 'panel:memory',
      group: 'panels',
      label: t('palette.toggleMemory'),
      run: () => memory.open(sessions.activeWorkspace?.path ?? '', sessions.activeWorkspace?.name),
    })
    cmds.push({
      id: 'panel:changes',
      group: 'panels',
      label: t('palette.openChanges'),
      keywords: 'diff review changes rollback',
      run: () => workspace.openChangesTab(),
    })
    cmds.push({
      id: 'panel:btw',
      group: 'panels',
      label: t('palette.openBtw'),
      keywords: 'btw side question',
      run: () => btw.show(),
    })
    cmds.push({
      id: 'panel:settings',
      group: 'panels',
      label: t('palette.toggleSettings'),
      run: () => (settings.panelOpen ? settings.closePanel() : settings.openPanel()),
    })

    for (const [m, label] of Object.entries(MODE_LABELS) as [PermissionMode, string][]) {
      cmds.push({
        id: `mode:${m}`,
        group: 'mode',
        label: t('palette.switchMode', { mode: label }),
        keywords: m,
        run: () => {
          void agent.setMode(m).catch((e) => toast.error(e))
        },
      })
    }

    cmds.push({
      id: 'appearance:theme',
      group: 'appearance',
      label: t('palette.toggleTheme'),
      keywords: 'dark light theme',
      run: () => settings.toggleDark(),
    })
    if (['running', 'awaiting', 'queued'].includes(sessions.activeStatus)) {
      cmds.push({
        id: 'action:interrupt',
        group: 'actions',
        label: t('palette.interrupt'),
        keywords: 'stop cancel interrupt',
        run: () => void sessions.cancel(),
      })
    }
    const lastAssistant = [...sessions.activeItems].reverse().find((i) => i.kind === 'assistant')
    if (lastAssistant && lastAssistant.kind === 'assistant') {
      const text = lastAssistant.content
      cmds.push({
        id: 'action:copyLast',
        group: 'actions',
        label: t('palette.copyLast'),
        keywords: 'copy reply clipboard',
        run: () => {
          void navigator.clipboard.writeText(text).then(() => toast.success(t('palette.copied')))
        },
      })
    }

    return cmds
  })
}
