import { describe, expect, it } from 'vitest'
import { markdownToPlain } from './markdown'

describe('markdownToPlain', () => {
  it('passes plain text through', () => {
    expect(markdownToPlain('just text')).toBe('just text')
  })
  it('drops heading and emphasis markers', () => {
    expect(markdownToPlain('## Title\n\n**bold** and *em* and ~~del~~')).toBe(
      'Title\n\nbold and em and del',
    )
  })
  it('renders links as label (href)', () => {
    expect(markdownToPlain('[docs](https://x.dev)')).toBe('docs (https://x.dev)')
  })
  it('keeps code blocks verbatim without fences', () => {
    expect(markdownToPlain('```js\nconst a = 1\n```')).toBe('const a = 1')
  })
  it('prefixes list items and indents nesting', () => {
    expect(markdownToPlain('- a\n- b')).toBe('- a\n- b')
    expect(markdownToPlain('1. a\n2. b')).toBe('1. a\n2. b')
  })
  it('joins table cells visibly', () => {
    expect(markdownToPlain('| a | b |\n| --- | --- |\n| 1 | 2 |')).toBe('a | b\n1 | 2')
  })
  it('prefixes blockquote lines', () => {
    expect(markdownToPlain('> quoted')).toBe('> quoted')
  })
  it('keeps hr and strips inline html', () => {
    expect(markdownToPlain('a\n\n---\n\n<b>bold</b>')).toBe('a\n\n---\n\nbold')
  })
  it('handles empty input', () => {
    expect(markdownToPlain('')).toBe('')
  })
  it('keeps image alts, hard breaks, and escapes', () => {
    expect(markdownToPlain('![logo](x.png)')).toBe('logo')
    expect(markdownToPlain('a  \nb')).toBe('a\nb')
    expect(markdownToPlain('\\*not em\\*')).toBe('*not em*')
  })
  it('keeps task list markers and nested indentation', () => {
    expect(markdownToPlain('- [x] done\n- [ ] todo')).toBe('- [x] done\n- [ ] todo')
    expect(markdownToPlain('- a\n  - b')).toBe('- a\n  - b')
  })
  it('renders bare autolinks without duplication', () => {
    expect(markdownToPlain('<https://x.dev>')).toBe('https://x.dev')
  })
  it('strips block-level html to its text', () => {
    expect(markdownToPlain('<div>x</div>\n\ntext')).toBe('x\n\ntext')
  })
})
