// The API facade. The implementation lives in focused modules —
// transport (request core + stream overrides), streaming (the SSE
// engines), workspace (session/workspace REST), chat (run/agent/memory
// REST), settings (the settings page's surface), bridge (the bridge
// product surface) — and everything re-exports here so the rest of the
// app keeps one stable import path (`../api/client`).
export * from './transport'
export * from './streaming'
export * from './workspace'
export * from './chat'
export * from './settings'
export * from './bridge'
