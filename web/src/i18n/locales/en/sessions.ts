export default {
  demo: {
    reply: '(Demo mode) This is a sample reply to "{prompt}" — no backend is connected.',
    codeBody: 'Streaming tokens, tool calls and results will appear here.',
    realMode: '- Real mode streams via `POST /api/runs` + SSE',
    toolCards: '- Tool calls and approvals render as cards/modals',
  },
  defaultWorkspace: 'Default workspace',
  newSession: 'New chat',
  imageMessage: 'Image message',
  sessionBusyDeleteFirst: 'The session is running — stop it before deleting',
  commands: {
    permissionSwitched:
      'Permission mode switched to {label} ({mode}); it applies to subsequent tool calls immediately.',
    permissionCurrent:
      'Current permission mode: {label} ({mode}).\nSwitch with /permissions <default {|} acceptEdits {|} bypassPermissions {|} readOnly {|} plan>.',
    thinkingSet: 'Thinking level set to {label} ({budget} tokens); it takes effect on the next turn.',
    thinkingCurrent:
      'Current thinking level: {label} ({budget} tokens).\nSwitch with /thinking <off {|} low {|} medium {|} high>.',
    failed: '⚠ Command failed: {message}',
  },
}
