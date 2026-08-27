// Status-grouped session list ("cockpit" sidebar): sessions from every
// workspace bucket into pinned / needs-input / running / today / earlier —
// state before content, the attention-heavy groups first.

export type SessionRunState = 'running' | 'awaiting' | ''

export type StatusGroupKey = 'pinned' | 'needsInput' | 'running' | 'today' | 'earlier'

export const STATUS_GROUP_ORDER: StatusGroupKey[] = ['pinned', 'needsInput', 'running', 'today', 'earlier']

export interface StatusRow<S extends { id: string; updatedAt: string }> {
  session: S
  workspace: string
  run: SessionRunState
}

export interface StatusGroup<S extends { id: string; updatedAt: string }> {
  key: StatusGroupKey
  rows: StatusRow<S>[]
}

export function bucketOf(pinned: boolean, run: SessionRunState, updatedAt: string, startOfToday: number): StatusGroupKey {
  if (pinned) return 'pinned'
  if (run === 'awaiting') return 'needsInput'
  if (run === 'running') return 'running'
  const ts = updatedAt ? new Date(updatedAt).getTime() : 0
  return ts >= startOfToday ? 'today' : 'earlier'
}

/** Buckets rows and drops empty groups; every bucket sorts newest-first. */
export function buildStatusGroups<S extends { id: string; updatedAt: string }>(
  rows: StatusRow<S>[],
  pinned: ReadonlySet<string>,
  now: Date = new Date(),
): StatusGroup<S>[] {
  const start = new Date(now)
  start.setHours(0, 0, 0, 0)
  const buckets = new Map<StatusGroupKey, StatusRow<S>[]>(STATUS_GROUP_ORDER.map((k) => [k, []]))
  for (const r of rows) {
    buckets.get(bucketOf(pinned.has(r.session.id), r.run, r.session.updatedAt, start.getTime()))!.push(r)
  }
  const byTime = (a: StatusRow<S>, b: StatusRow<S>) =>
    b.session.updatedAt.localeCompare(a.session.updatedAt)
  return STATUS_GROUP_ORDER.map((key) => ({ key, rows: (buckets.get(key) ?? []).sort(byTime) })).filter(
    (g) => g.rows.length > 0,
  )
}
