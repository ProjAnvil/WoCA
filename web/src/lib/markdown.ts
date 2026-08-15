import { marked } from 'marked'
import DOMPurify from 'dompurify'

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
