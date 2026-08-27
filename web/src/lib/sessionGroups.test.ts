import { describe, expect, it } from 'vitest'
import { buildStatusGroups, type StatusRow } from './sessionGroups'

interface S {
  id: string
  updatedAt: string
}

function row(id: string, updatedAt: string, run: StatusRow<S>['run'] = '', workspace = 'ws'): StatusRow<S> {
  return { session: { id, updatedAt }, workspace, run }
}

// 2026-08-27 12:00 local — "today" boundary math runs against this clock.
const NOW = new Date('2026-08-27T12:00:00')

describe('buildStatusGroups', () => {
  it('buckets by status with pinned winning over run state', () => {
    const groups = buildStatusGroups(
      [
        row('a', '2026-08-27T10:00:00', 'awaiting'),
        row('f', '2026-08-27T07:00:00', 'awaiting'),
        row('b', '2026-08-27T11:00:00', 'running'),
        row('c', '2026-08-27T09:00:00'),
        row('d', '2026-08-20T09:00:00'),
        row('e', '2026-08-26T09:00:00', 'running'),
      ],
      new Set(['a']),
      NOW,
    )
    expect(groups.map((g) => g.key)).toEqual(['pinned', 'needsInput', 'running', 'today', 'earlier'])
    expect(groups.find((g) => g.key === 'pinned')!.rows.map((r) => r.session.id)).toEqual(['a'])
    expect(groups.find((g) => g.key === 'needsInput')!.rows.map((r) => r.session.id)).toEqual(['f'])
    expect(groups.find((g) => g.key === 'running')!.rows.map((r) => r.session.id)).toEqual(['b', 'e'])
    expect(groups.find((g) => g.key === 'today')!.rows.map((r) => r.session.id)).toEqual(['c'])
    expect(groups.find((g) => g.key === 'earlier')!.rows.map((r) => r.session.id)).toEqual(['d'])
  })

  it('drops empty groups entirely', () => {
    const groups = buildStatusGroups([row('x', '2026-08-27T08:00:00')], new Set(), NOW)
    expect(groups.map((g) => g.key)).toEqual(['today'])
  })

  it('sorts every bucket newest-first', () => {
    const groups = buildStatusGroups(
      [row('old', '2026-08-26T10:00:00'), row('newer', '2026-08-26T18:00:00'), row('newest', '2026-08-27T08:00:00')],
      new Set(),
      NOW,
    )
    expect(groups.find((g) => g.key === 'today')!.rows.map((r) => r.session.id)).toEqual(['newest'])
    expect(groups.find((g) => g.key === 'earlier')!.rows.map((r) => r.session.id)).toEqual(['newer', 'old'])
  })

  it('yesterday evening lands in earlier, this morning in today', () => {
    const groups = buildStatusGroups(
      [row('late-night', '2026-08-26T23:30:00'), row('early', '2026-08-27T00:05:00')],
      new Set(),
      NOW,
    )
    expect(groups.find((g) => g.key === 'today')!.rows.map((r) => r.session.id)).toEqual(['early'])
    expect(groups.find((g) => g.key === 'earlier')!.rows.map((r) => r.session.id)).toEqual(['late-night'])
  })
})
