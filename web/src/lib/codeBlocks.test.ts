import { beforeEach, describe, expect, it, vi } from 'vitest'
import { renderMarkdown } from './markdown'
import { decorateCodeBlocks, highlightCodeBlocks } from './codeBlocks'

vi.mock('./highlight', () => ({
  highlightCode: vi.fn(async (code: string, lang: string) =>
    lang === 'go' ? `<pre class="shiki"><code><span style="color:#005CC5">${code.trim()}</span></code></pre>` : null,
  ),
}))

import { highlightCode } from './highlight'

const labels = { copy: 'Copy code', copied: 'Code copied' }

beforeEach(() => {
  vi.mocked(highlightCode).mockClear()
})

// A bare fenced block as the very first node loses its <pre> wrapper in
// DOMPurify-under-happy-dom (parser quirk; browsers keep it), so every
// fixture carries a leading paragraph — the realistic chat shape anyway.
const md = (code: string) => `intro\n\n${code}`

function makeRoot(source: string) {
  const root = document.createElement('div')
  root.innerHTML = renderMarkdown(source)
  decorateCodeBlocks(root, labels)
  return root
}

describe('decorateCodeBlocks', () => {
  it('wraps fenced blocks with a language header and copy button', () => {
    const root = makeRoot(md('```go\nfmt.Println(1)\n```'))
    decorateCodeBlocks(root, labels)
    expect(root.querySelector('.code-block .code-head .code-lang')?.textContent).toBe('go')
    expect(root.querySelector('.code-block .code-copy')).toBeTruthy()
  })
  it('labels untagged blocks as text', () => {
    const root = makeRoot(md('```\nplain\n```'))
    decorateCodeBlocks(root, labels)
    expect(root.querySelector('.code-lang')?.textContent).toBe('text')
  })
  it('is idempotent', () => {
    const root = makeRoot(md('```go\nx\n```'))
    decorateCodeBlocks(root, labels)
    decorateCodeBlocks(root, labels)
    expect(root.querySelectorAll('.code-block')).toHaveLength(1)
  })
  it('delegated click copies the raw code', async () => {
    const write = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: write }, configurable: true })
    const root = makeRoot(md('```go\nfmt.Println(1)\n```'))
    decorateCodeBlocks(root, labels)
    root.querySelector('.code-copy')!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await Promise.resolve()
    expect(write).toHaveBeenCalledWith('fmt.Println(1)\n')
  })
})

describe('highlightCodeBlocks', () => {
  const flush = async () => {
    await Promise.resolve()
    await Promise.resolve()
  }

  it('injects shiki spans and scopes dark-mode switching', async () => {
    const root = makeRoot(md('```go\nfmt.Println(1)\n```'))
    highlightCodeBlocks(root)
    await flush()
    const code = root.querySelector('.code-block code')!
    expect(code.innerHTML).toContain('color:#005CC5')
    expect(code.classList).toContain('shiki-host')
  })

  it('skips untagged/text blocks', async () => {
    const root = makeRoot(md('```\nplain\n```'))
    highlightCodeBlocks(root)
    await flush()
    expect(highlightCode).not.toHaveBeenCalled()
    expect(root.querySelector('.code-block code')!.textContent).toBe('plain\n')
  })

  it('skips oversized blocks (>200 lines)', async () => {
    const long = Array.from({ length: 201 }, (_, i) => `line ${i}`).join('\n')
    const root = makeRoot(md('```go\n' + long + '\n```'))
    highlightCodeBlocks(root)
    await flush()
    expect(highlightCode).not.toHaveBeenCalled()
  })

  it('is idempotent across repeated calls', async () => {
    const root = makeRoot(md('```go\nx\n```'))
    highlightCodeBlocks(root)
    await flush()
    highlightCodeBlocks(root)
    await flush()
    expect(highlightCode).toHaveBeenCalledTimes(1)
  })
})
