import { describe, expect, it } from 'vitest'
import { toolEventSummary } from './toolEvent'

describe('toolEventSummary', () => {
  it('prefers identifying keys over the first arbitrary string', () => {
    expect(toolEventSummary('Read', { note: 'hello', file: 'src/main.ts' })).toBe('Read · src/main.ts')
    expect(toolEventSummary('Bash', { cmd: 'go build ./...' })).toBe('Bash · go build ./...')
  })

  it('falls back to the first string value, then the bare name', () => {
    expect(toolEventSummary('Grep', { q: 'pattern-x' })).toBe('Grep · pattern-x')
    expect(toolEventSummary('Exit', { code: 0 })).toBe('Exit')
    expect(toolEventSummary('NoArgs', undefined)).toBe('NoArgs')
  })

  it('truncates long single-line and multiline values', () => {
    const long = 'a'.repeat(80)
    expect(toolEventSummary('Edit', { file: long })).toBe(`Edit · ${'a'.repeat(45)}…`)
    expect(toolEventSummary('Bash', { cmd: 'echo hi\necho bye' })).toBe('Bash · echo hi')
  })

  it('ignores blank strings', () => {
    expect(toolEventSummary('Read', { file: '   ' })).toBe('Read')
  })
})
