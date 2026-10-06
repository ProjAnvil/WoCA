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
