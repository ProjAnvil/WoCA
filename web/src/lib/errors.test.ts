import { describe, expect, it } from 'vitest'
import { toErrorMessage } from './errors'

describe('toErrorMessage', () => {
  it('uses the message of real Errors', () => {
    expect(toErrorMessage(new Error('boom'))).toBe('boom')
  })

  it('stringifies non-Error values thrown across boundaries', () => {
    expect(toErrorMessage('plain string')).toBe('plain string')
    expect(toErrorMessage(42)).toBe('42')
    expect(toErrorMessage(undefined)).toBe('undefined')
    expect(toErrorMessage(null)).toBe('null')
  })
})
