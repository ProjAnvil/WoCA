import { marked } from 'marked'
import DOMPurify from 'dompurify'
import type { Token, Tokens } from 'marked'

/**
 * Render untrusted Markdown to sanitized HTML.
 *
 * Model output, WebFetch results and tool output are all untrusted, so we
 * sanitize the rendered HTML before injecting it into the DOM (v-html).
 */
export function renderMarkdown(src: string): string {
  const html = marked.parse(src, { async: false, gfm: true }) as string
  return DOMPurify.sanitize(html)
}

/**
 * Markdown source → plain text ("copy as plain text"): marked-lexer walk,
 * no DOM dependency. Structural markers survive as their readable forms
 * (lists keep bullets, links become "label (href)", tables join cells).
 */
export function markdownToPlain(src: string): string {
  return blocks(marked.lexer(src)).replace(/\n{3,}/g, '\n\n').trim()
}

function blocks(tokens: Token[]): string {
  const parts: string[] = []
  for (const tk of tokens) {
    switch (tk.type) {
      case 'space':
        continue
      case 'heading':
      case 'paragraph':
        parts.push(inline((tk as Tokens.Heading).tokens ?? []))
        break
      case 'code':
        parts.push((tk as Tokens.Code).text)
        break
      case 'blockquote':
        parts.push(
          blocks((tk as Tokens.Blockquote).tokens ?? [])
            .split('\n')
            .map((l) => `> ${l}`)
            .join('\n'),
        )
        break
      case 'list':
        parts.push(
          ((tk as Tokens.List).items ?? [])
            .map((it, i) => listItem(it, i, 0, (tk as Tokens.List).ordered))
            .join('\n'),
        )
        break
      case 'table': {
        const tb = tk as Tokens.Table
        const rows = [tb.header, ...tb.rows].map((cells) =>
          cells.map((c) => inline(c.tokens ?? [])).join(' | '),
        )
        parts.push(rows.join('\n'))
        break
      }
      case 'hr':
        parts.push('---')
        break
      case 'html':
        parts.push((tk as Tokens.HTML).text.replace(/<[^>]*>/g, ''))
        break
      default:
        if ('text' in tk) parts.push(String((tk as { text: string }).text))
    }
  }
  return parts.filter((p) => p !== '').join('\n\n')
}

function listItem(it: Tokens.ListItem, index: number, depth: number, ordered: boolean): string {
  const pad = '  '.repeat(depth)
  const marker = it.task ? `- [${it.checked ? 'x' : ' '}] ` : `${ordered ? `${index + 1}.` : '-'} `
  // Nested blocks (a nested list) join without paragraph gaps so the
  // indented child lines stay adjacent to their parent.
  return blocks((it.tokens ?? []) as Token[])
    .replace(/\n{2,}/g, '\n')
    .split('\n')
    .map((l, i) => (i === 0 ? `${pad}${marker}${l}` : `${pad}  ${l}`))
    .join('\n')
}

function inline(tokens: Token[]): string {
  let out = ''
  for (const tk of tokens) {
    switch (tk.type) {
      case 'escape':
      case 'text': {
        const t = tk as Tokens.Text
        out += t.tokens?.length ? inline(t.tokens) : t.text
        break
      }
      case 'strong':
      case 'em':
      case 'del':
        out += inline((tk as Tokens.Strong).tokens ?? [])
        break
      case 'link': {
        const l = tk as Tokens.Link
        const label = inline(l.tokens ?? [])
        out += label === l.href ? label : `${label} (${l.href})`
        break
      }
      case 'image':
        out += (tk as Tokens.Image).text || ''
        break
      case 'codespan':
        out += (tk as Tokens.Codespan).text
        break
      case 'br':
        out += '\n'
        break
      case 'html':
        out += (tk as Tokens.HTML).text.replace(/<[^>]*>/g, '')
        break
      default:
        if ('raw' in tk) out += String((tk as { raw: string }).raw)
        else if ('text' in tk) out += String((tk as { text: string }).text)
    }
  }
  return out
}
