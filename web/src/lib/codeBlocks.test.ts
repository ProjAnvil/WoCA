import { describe, expect, it, vi } from 'vitest'
import { renderMarkdown } from './markdown'
import { decorateCodeBlocks } from './codeBlocks'

const labels = { copy: 'Copy code', copied: 'Code copied' }

// A bare fenced block as the very first node loses its <pre> wrapper in
// DOMPurify-under-happy-dom (parser quirk; browsers keep it), so every
// fixture carries a leading paragraph — the realistic chat shape anyway.
const md = (code: string) => `intro\n\n${code}`

function makeRoot(source: string) {
  const root = document.createElement('div')
  root.innerHTML = renderMarkdown(source)
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
