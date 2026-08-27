// Token discipline check: outside styles/tokens.css no component/style file
// may introduce bare hex colors — semantic vars only. Current leftovers are
// ratcheted per file (line counts): the allowed numbers may only shrink.
// Run via `npm run check:tokens`.
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join } from 'node:path'

const ROOT = new URL('../src', import.meta.url).pathname
const TOKENS_FILE = 'src/styles/tokens.css'

// Deliberate holdouts, all scheduled for the P1 gate-visual convergence
// (see woca-internal-docs/web-ui-ux-overhaul.md §2/P0-1): the native-gate
// gradient palette and gate brand surfaces.
const ALLOWED = {
  // The native-gate gradient palette split across section files after the
  // style.css breakup — same 36 lines as before, regrouped.
  'src/styles/chat.css': 5,
  'src/styles/panels.css': 1,
  'src/styles/misc.css': 30,
  'src/native/ConnectView.vue': 2,
  'src/components/LoginGate.vue': 1,
}

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
