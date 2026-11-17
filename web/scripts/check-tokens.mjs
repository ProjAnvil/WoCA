// Token discipline check: outside styles/tokens.css no component/style file
// may introduce bare hex colors — semantic vars only. Current leftovers are
// ratcheted per file (line counts): the allowed numbers may only shrink.
// Additionally the ALLOWED exemption list itself is frozen (see ALLOWED_FREEZE
// below): growing it fails the run. Run via `npm run check:tokens`.
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join } from 'node:path'

const ROOT = new URL('../src', import.meta.url).pathname
const TOKENS_FILE = 'src/styles/tokens.css'

// Deliberate holdouts, all scheduled for the P1 gate-visual convergence
// (see woca-internal-docs/web-ui-ux-overhaul.md §2/P0-1): the native-gate
// gradient palette and gate brand surfaces.
// Re-baselined 2026-11-18 (UI follow-ups F5) to the actually-scanned state:
// the style.css breakup cleanups had already shrunk misc.css and ConnectView,
// and WebSearchSection's --fg-dim fallbacks predate the freeze — grandfathered
// as-is, zero new lines admitted.
const ALLOWED = {
  // The native-gate gradient palette split across section files after the
  // style.css breakup — regrouped, shrunk from the original counts.
  'src/styles/chat.css': 5,
  'src/styles/panels.css': 1,
  'src/styles/misc.css': 4,
  'src/components/settings/WebSearchSection.vue': 3, // --fg-dim hex fallbacks
  'src/components/LoginGate.vue': 1, // QR scan substrate white
}

// Ratchet freeze (F5, 2026-11-18): canonical snapshot of ALLOWED at freeze
// time — sorted `file:count` pairs, 5 files / 14 grandfathered lines. The
// ratchet only turns one way:
//   - a NEW violation anywhere (file not covered, or more hex lines than the
//     entry allows) fails the scan below — zero tolerance;
//   - GROWING ALLOWED past this snapshot (new entry, raised count, renamed
//     path) fails the freeze check — grandfathering is closed;
//   - shrinking (lower count / dropped entry) passes; tighten this snapshot
//     in the same commit when you do, so the ceiling stays exact.
const ALLOWED_FREEZE =
  'src/components/LoginGate.vue:1,' +
  'src/components/settings/WebSearchSection.vue:3,' +
  'src/styles/chat.css:5,' +
  'src/styles/misc.css:4,' +
  'src/styles/panels.css:1'

const FREEZE = Object.fromEntries(
  ALLOWED_FREEZE.split(',').map((pair) => {
    const i = pair.lastIndexOf(':')
    return [pair.slice(0, i), Number(pair.slice(i + 1))]
  }),
)

const HEX_LINE = /#[0-9a-fA-F]{3,8}\b/

function walk(dir) {
  const out = []
  for (const name of readdirSync(dir)) {
    const full = join(dir, name)
    if (statSync(full).isDirectory()) out.push(...walk(full))
    else if (/\.(vue|css)$/.test(name)) out.push(full)
  }
  return out
}

const failures = []

// Freeze assertion: ALLOWED may not grow past ALLOWED_FREEZE. Missing keys
// count as -1 so any brand-new entry is growth; renames are new keys too.
const growth = Object.keys(ALLOWED).filter((id) => ALLOWED[id] > (FREEZE[id] ?? -1))
if (growth.length) {
  failures.push(
    `ALLOWED exemption list grew past the frozen ratchet (F5, 2026-11-18) for: ${growth.join(', ')}\n` +
      `  frozen snapshot: ${ALLOWED_FREEZE}\n` +
      `  Grandfathering is closed: new bare-hex lines are never exemptible\n` +
      `  (register a token in styles/tokens.css instead). Lowering counts or\n` +
      `  dropping entries is fine — tighten ALLOWED_FREEZE in the same commit.`
  )
}
// Non-fatal: ALLOWED fell below the ceiling — invite tightening the snapshot.
const shrink = Object.keys(FREEZE).filter((id) => (ALLOWED[id] ?? 0) < FREEZE[id])
if (shrink.length && !growth.length) {
  console.log(
    'note: ALLOWED is below the frozen ceiling (good) — tighten ALLOWED_FREEZE for: ' +
      shrink.join(', ')
  )
}

const remaining = {}
for (const abs of walk(ROOT)) {
  const rel = abs.slice(ROOT.length + 1)
  const id = `src/${rel}`
  if (id === TOKENS_FILE) continue
  const hits = readFileSync(abs, 'utf8')
    .split('\n')
    .filter((l) => HEX_LINE.test(l)).length
  if (!hits) continue
  remaining[id] = hits
  const allowed = ALLOWED[id] ?? 0
  if (hits > allowed) failures.push(`${id}: ${hits} bare-hex lines (allowed ${allowed})`)
}

console.log('bare-hex ratchet:', JSON.stringify(remaining))
if (failures.length) {
  console.error('token discipline violations:\n' + failures.join('\n'))
  process.exit(1)
}
console.log('check:tokens ok')
