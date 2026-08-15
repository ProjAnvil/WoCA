import type { HighlighterCore } from 'shiki/core'

// shiki (the code highlighter used by VitePress/Nuxt), lazily created on the
// first preview. Dual github themes let the preview follow the light/dark
// toggle via the CSS vars swapped in style.css.
let highlighterPromise: Promise<HighlighterCore> | null = null

function getHighlighter(): Promise<HighlighterCore> {
  if (!highlighterPromise) {
    highlighterPromise = import('./highlighter-engine').then((m) => m.buildHighlighter())
  }
  return highlighterPromise
}

const EXT_TO_LANG: Record<string, string> = {
  go: 'go',
  mod: 'go',
  ts: 'typescript',
  mts: 'typescript',
  tsx: 'tsx',
  js: 'javascript',
  mjs: 'javascript',
  cjs: 'javascript',
  jsx: 'jsx',
  vue: 'vue',
  json: 'json',
  yaml: 'yaml',
  yml: 'yaml',
  sh: 'bash',
  bash: 'bash',
  zsh: 'bash',
  fish: 'bash',
  py: 'python',
  css: 'css',
  scss: 'css',
  html: 'html',
  htm: 'html',
  md: 'markdown',
  mdx: 'markdown',
  markdown: 'markdown',
  sql: 'sql',
  xml: 'xml',
  rs: 'rust',
  java: 'java',
  toml: 'toml',
  c: 'c',
  h: 'c',
  cpp: 'cpp',
  cc: 'cpp',
  cxx: 'cpp',
  hpp: 'cpp',
  diff: 'diff',
  patch: 'diff',
}

/** Maps a file name to a shiki language id ('text' fallback). */
export function langForFile(name: string): string {
  const lower = name.toLowerCase()
  if (lower.startsWith('dockerfile')) return 'bash'
  if (lower === 'makefile' || lower.endsWith('.mk')) return 'bash'
  const ext = lower.includes('.') ? lower.slice(lower.lastIndexOf('.') + 1) : ''
  return EXT_TO_LANG[ext] ?? 'text'
}

/**
 * Highlights code to trusted HTML (shiki escapes the source). Returns null
 * when the highlighter is unavailable so callers can render a plain <pre>.
 */
export async function highlightCode(code: string, lang: string): Promise<string | null> {
  try {
    const hl = await getHighlighter()
    return hl.codeToHtml(code, {
      lang,
      themes: { light: 'github-light', dark: 'github-dark' },
    })
  } catch {
    return null
  }
}
