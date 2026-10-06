import { highlightCode } from './highlight'

export interface CodeCopyLabels {
  copy: string
  copied: string
}

const ICONS = `<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg><svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M20 6 9 17l-5-5"/></svg>`

/**
 * Post-render DOM decoration for fenced code blocks: wraps each <pre> in a
 * .code-block with a language header and a copy button. Runs AFTER
 * sanitization on the live DOM (never inside the sanitized HTML string —
 * v-html replaces it every tick) and binds one delegated click listener per
 * container. Idempotent; call again after content swaps.
 */
export function decorateCodeBlocks(root: HTMLElement, labels: CodeCopyLabels): void {
  if (!root.dataset.codeCopyBound) {
    root.dataset.codeCopyBound = '1'
    root.addEventListener('click', (e) => {
      const btn = (e.target as HTMLElement).closest<HTMLButtonElement>('.code-copy')
      if (!btn) return
      const code = btn.closest('.code-block')?.querySelector('pre code')
      if (!code) return
      void navigator.clipboard.writeText(code.textContent ?? '').then(() => {
        btn.classList.add('copied')
        btn.setAttribute('aria-label', labels.copied)
        window.setTimeout(() => {
          btn.classList.remove('copied')
          btn.setAttribute('aria-label', labels.copy)
        }, 1800)
      })
    })
  }
  for (const pre of Array.from(root.querySelectorAll('pre'))) {
    if (pre.parentElement?.classList.contains('code-block')) continue
    const code = pre.querySelector('code')
    const lang =
      Array.from(code?.classList ?? [])
        .find((c) => c.startsWith('language-'))
        ?.slice('language-'.length) ?? ''
    const wrap = document.createElement('div')
    wrap.className = 'code-block'
    const head = document.createElement('div')
    head.className = 'code-head'
    const label = document.createElement('span')
    label.className = 'code-lang'
    label.textContent = lang || 'text'
    const btn = document.createElement('button')
    btn.type = 'button'
    btn.className = 'code-copy'
    btn.setAttribute('aria-label', labels.copy)
    btn.innerHTML = ICONS
    head.append(label, btn)
    pre.replaceWith(wrap)
    wrap.append(head, pre)
  }
}

/** Fuses: single blocks over this many lines, or whole containers past this
 * many bytes, stay unhighlighted (re-parse cost outweighs the value). */
const HL_MAX_LINES = 200
const HL_MAX_TOTAL_BYTES = 64_000

/**
 * Syntax-highlight already-decorated chat code blocks (same lazy shiki
 * engine + dual github themes as the file preview). Spans are injected on
 * the live DOM after sanitization; the .shiki-host class scopes the
 * dark-mode color swap from panels.css. Idempotent; async per block — a
 * write landing on a node that v-html has since replaced is inert.
 */
export function highlightCodeBlocks(root: HTMLElement): void {
  const codes = Array.from(root.querySelectorAll<HTMLElement>('.code-block pre code'))
  let budget = HL_MAX_TOTAL_BYTES
  for (const code of codes) {
    if (code.dataset.hlDone === '1') continue
    code.dataset.hlDone = '1'
    const text = code.textContent ?? ''
    budget -= text.length
    const lang =
      Array.from(code.classList)
        .find((c) => c.startsWith('language-'))
        ?.slice('language-'.length) ?? ''
    const lines = text.split('\n').length
    if (!lang || lang === 'text' || lines > HL_MAX_LINES || budget < 0) continue
    void highlightCode(text, lang).then((html) => {
      if (!html) return
      const tpl = document.createElement('template')
      tpl.innerHTML = html
      const inner = tpl.content.querySelector('code')
      if (inner) {
        code.innerHTML = inner.innerHTML
        code.classList.add('shiki-host')
      }
    })
  }
}
