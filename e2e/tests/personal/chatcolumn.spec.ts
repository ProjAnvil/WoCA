import { spawn, type ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import net from 'node:net';
import path from 'node:path';
import { test, expect, type Page } from '@playwright/test';
import { pinLocale } from '../../fixtures/lab';
import { repoRoot, runtimeDir } from '../../fixtures/paths';

// Form 1 — the F3 wide-screen reading column against a REAL `woca serve`
// binary. Pins the --chat-col tier change (plan task 6, spec I3 ruling):
// >=1440 and >=1800 both serve a 760px column (the deliberate "wide
// screens read narrow by design" cap; 860 at the 1440 crossing is the
// intended semantics, consistent with the 2026-11-17 audit P1-1
// line-length criticism), while the 860px base tier and everything below
// 1440 stay put — stopnotice.spec.ts pins the same 860 tier at 1024 from
// its own conversation, this spec re-pins it from an independent one.
//
// The tier is a single variable consumed by nine aligned surfaces
// (.message, .stream-card, .msg-skeleton-wrap, .stream-liveness,
// .queue-badge, .chat-input-inner, .input-toolbar, .attach-tray,
// .ac-popup): no consumer selector changes, so asserting the computed
// tier on .message (transcript) AND .chat-input-inner (composer) — with
// their rendered boxes equal within 1px — anchors the three-edge
// alignment contract for all of them.
//
// Dedicated instance: same isolation rationale as stopnotice.spec.ts —
// this spec reconfigures the provider (PUT /api/settings is server-global
// and would race the parallel personal specs on the shared server) and
// wants its own sidebar group, so it spawns its OWN `woca serve` (the
// binary the playwright webServer builds at e2e/.bin/woca) with an
// isolated WOCA_HOME. The scripted model is a fast plain OpenAI-compatible
// stand-in: every call gets the same reply (markdown link + code fence,
// so the seeded conversation doubles as the visual content for the task's
// 1440 zh/en screenshot spot check).
//
// The seed run is API-driven (POST /api/runs carrying the workspace —
// the workspace-bearing run contract) so each viewport test opens a fresh
// page against one deterministic conversation and the four tier
// assertions stay independent (a wide-viewport failure must not mask the
// 1024/375 passes).

const MARKER = 'WIDE-COLUMN';
const PROMPT = `${MARKER} reply with the fixture markdown`;
const REPLY_MARKER = 'wide screens read narrow by design';
const REPLY = [
  'Column fixture reply with a [woca link](https://example.com/woca), inline `code`,',
  'and a fenced block:',
  '',
  '```go',
  'func main() {',
  `\tfmt.Println("${REPLY_MARKER}")`,
  '}',
  '```',
].join('\n');

let modelURL = '';
let modelServer: http.Server | undefined;
let serve: ChildProcess | undefined;
let base = '';

/** One SSE chunk in the chat-completions streaming shape. */
function chunk(delta: Record<string, unknown>, finish: string | null): string {
  return (
    'data: ' +
    JSON.stringify({
      id: 'chatcmpl-e2e',
      object: 'chat.completion.chunk',
      choices: [{ index: 0, delta, finish_reason: finish }],
    }) +
    '\n\n'
  );
}

/** A free loopback port (bind :0, read it, release — good enough locally). */
function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address();
      const port = addr && typeof addr === 'object' ? addr.port : 0;
      srv.close(() => (port ? resolve(port) : reject(new Error('no free port'))));
    });
    srv.on('error', reject);
  });
}

/** Spawns the dedicated instance and waits for its /api/health to answer. */
async function startServe(): Promise<{ child: ChildProcess; base: string }> {
  const bin = path.join(repoRoot, 'e2e', '.bin', 'woca');
  if (!fs.existsSync(bin)) {
    throw new Error(`${bin} missing — the playwright webServer builds it before tests run`);
  }
  const home = path.join(runtimeDir, 'chatcolumn', `home-${process.pid}-${Date.now()}`);
  fs.mkdirSync(home, { recursive: true });
  const port = await freePort();
  const child = spawn(
    bin,
    ['serve', '--addr', `127.0.0.1:${port}`, '--no-browser', '--log-level', 'warn'],
    {
      env: {
        ...process.env,
        HOME: home,
        WOCA_HOME: home,
        WOCA_DB: path.join(home, 'woca.db'),
        WOCA_PLUGIN_ROOT: path.join(home, 'plugins'),
        WOCA_NO_BROWSER: '1',
      },
      stdio: ['ignore', 'ignore', 'pipe'],
    },
  );
  const stderr: string[] = [];
  child.stderr?.on('data', (d: Buffer) => stderr.push(d.toString()));
  const at = `http://127.0.0.1:${port}`;
  const deadline = Date.now() + 30_000;
  for (;;) {
    if (child.exitCode !== null) {
      throw new Error(`dedicated woca serve exited early (code ${child.exitCode}): ${stderr.join('').slice(-500)}`);
    }
    try {
      const res = await fetch(`${at}/api/health`);
      if (res.ok) break;
    } catch {
      // not up yet
    }
    if (Date.now() > deadline) {
      child.kill('SIGKILL');
      throw new Error(`dedicated woca serve never became healthy: ${stderr.join('').slice(-500)}`);
    }
    await new Promise((r) => setTimeout(r, 200));
  }
  return { child, base: at };
}

test.beforeAll(async () => {
  // The scripted model: one fast reply for every caller (streaming or not,
  // including the session-title summarizer).
  modelServer = http.createServer((req, res) => {
    if (!req.url?.includes('/chat/completions')) {
      res.writeHead(404).end();
      return;
    }
    const parts: Buffer[] = [];
    req.on('data', (b: Buffer) => parts.push(b));
    req.on('end', () => {
      let stream = true;
      try {
        stream = (JSON.parse(Buffer.concat(parts).toString('utf8')) as { stream?: boolean }).stream === true;
      } catch {
        stream = true;
      }
      if (!stream) {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(
          JSON.stringify({
            id: 'chatcmpl-e2e',
            object: 'chat.completion',
            choices: [{ index: 0, message: { role: 'assistant', content: REPLY }, finish_reason: 'stop' }],
          }),
        );
        return;
      }
      res.writeHead(200, { 'Content-Type': 'text/event-stream' });
      res.write(chunk({ role: 'assistant', content: REPLY }, null));
      res.write(chunk({}, 'stop'));
      res.end('data: [DONE]\n\n');
    });
  });
  await new Promise<void>((resolve) => modelServer!.listen(0, '127.0.0.1', resolve));
  const addr = modelServer!.address();
  if (!addr || typeof addr !== 'object') throw new Error('fake model has no address');
  modelURL = `http://127.0.0.1:${addr.port}`;

  // The dedicated personal instance.
  const started = await startServe();
  serve = started.child;
  base = started.base;

  // Seed once for all four viewport tests: configure the scripted model,
  // bypass the approval gates (the subject is layout, not permissions),
  // then drive one workspace-bearing run through the API.
  const put = await fetch(`${base}/api/settings`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      provider: 'openai',
      model: 'gpt-e2e',
      openai_base_url: `${modelURL}/v1`,
      openai_api_key: 'e2e-key',
    }),
  });
  if (!put.ok) throw new Error(`settings PUT failed: ${put.status}`);
  const perms = await fetch(`${base}/api/agent/permissions`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ mode: 'bypassPermissions' }),
  });
  if (!perms.ok) throw new Error(`permissions POST failed: ${perms.status}`);

  // A fresh workspace dir under e2e/.runtime: the seeded session gives the
  // workspace its sidebar group (the fresh-install posture stopnotice
  // established).
  const ws = path.join(runtimeDir, 'chatcolumn', `ws-${process.pid}-${Date.now()}`);
  fs.mkdirSync(ws, { recursive: true });
  const seed = await fetch(`${base}/api/sessions`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ workspace: ws }),
  });
  if (!seed.ok) throw new Error(`session seed failed: ${seed.status}`);
  const seeded = (await seed.json()) as { session_id: string; workspace: string };
  if (!seeded.session_id || !seeded.workspace) throw new Error('session seed response incomplete');

  const run = await fetch(`${base}/api/runs`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ session_id: seeded.session_id, prompt: PROMPT, workspace: seeded.workspace }),
  });
  if (!run.ok) throw new Error(`seed run failed: ${run.status}`);

  // Wait for the round to persist: the width tests need .message bubbles,
  // which render from the persisted transcript.
  const deadline = Date.now() + 30_000;
  for (;;) {
    const res = await fetch(`${base}/api/sessions/${seeded.session_id}`);
    const { messages } = (await res.json()) as { messages: Array<{ content?: string }> };
    if (messages.some((m) => (m.content ?? '').includes(REPLY_MARKER))) break;
    if (Date.now() > deadline) throw new Error('seeded run never persisted its reply');
    await new Promise((r) => setTimeout(r, 200));
  }
});

test.afterAll(async () => {
  const child = serve;
  if (child) {
    child.kill('SIGTERM');
    await new Promise<void>((resolve) => {
      const t = setTimeout(() => {
        child.kill('SIGKILL');
        resolve();
      }, 5_000);
      child.once('exit', () => {
        clearTimeout(t);
        resolve();
      });
    });
  }
  if (modelServer) {
    modelServer.closeAllConnections?.();
    await new Promise<void>((r) => modelServer!.close(() => r()));
  }
});

/** Opens the seeded conversation: the dedicated instance boots into its
 * only session; wait for the seeded prompt bubble so a slow boot cannot
 * race the width probes. */
async function openSeededChat(page: Page): Promise<void> {
  await pinLocale(page);
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto(base);
  await expect(page.locator('aside.sidebar')).toBeVisible();
  await expect(page.locator('.message.user').first()).toContainText(MARKER);
  await expect(page.locator('.message.assistant .markdown-body')).toContainText(REPLY_MARKER);
}

/** Computed tier + rendered box widths of the two alignment anchors. */
async function columnWidths(page: Page): Promise<{
  msgMax: string;
  inputMax: string;
  msgBox: number;
  inputBox: number;
  docScroll: number;
  inner: number;
}> {
  return page.evaluate(() => {
    const msg = document.querySelector('.message') as HTMLElement | null;
    const input = document.querySelector('.chat-input-inner') as HTMLElement | null;
    if (!msg || !input) throw new Error('column elements missing');
    return {
      msgMax: getComputedStyle(msg).maxWidth,
      inputMax: getComputedStyle(input).maxWidth,
      msgBox: msg.getBoundingClientRect().width,
      inputBox: input.getBoundingClientRect().width,
      docScroll: document.documentElement.scrollWidth,
      inner: window.innerWidth,
    };
  });
}

test('1440px viewport: chat column tier narrows to 760', async ({ page }) => {
  await openSeededChat(page);
  await page.setViewportSize({ width: 1440, height: 900 });
  const w = await columnWidths(page);
  console.log('1440 viewport column widths', JSON.stringify(w));
  expect(w.msgMax, 'message column tier at 1440').toBe('760px');
  expect(w.inputMax, 'composer column tier at 1440').toBe('760px');
  expect(w.msgBox, 'message box is at most the 760 tier').toBeLessThanOrEqual(760);
  expect(w.inputBox, 'composer box is at most the 760 tier').toBeLessThanOrEqual(760);
  expect(Math.abs(w.msgBox - w.inputBox), 'message/composer three-edge alignment').toBeLessThanOrEqual(1);
});

test('1800px viewport: chat column tier caps at 760', async ({ page }) => {
  await openSeededChat(page);
  await page.setViewportSize({ width: 1800, height: 1000 });
  const w = await columnWidths(page);
  console.log('1800 viewport column widths', JSON.stringify(w));
  expect(w.msgMax, 'message column tier at 1800').toBe('760px');
  expect(w.inputMax, 'composer column tier at 1800').toBe('760px');
  expect(w.msgBox, 'message box is at most the 760 cap').toBeLessThanOrEqual(760);
  expect(w.inputBox, 'composer box is at most the 760 cap').toBeLessThanOrEqual(760);
  expect(Math.abs(w.msgBox - w.inputBox), 'message/composer three-edge alignment').toBeLessThanOrEqual(1);
});

test('1024px viewport: base tier stays 860', async ({ page }) => {
  await openSeededChat(page);
  await page.setViewportSize({ width: 1024, height: 768 });
  const w = await columnWidths(page);
  console.log('1024 viewport column widths', JSON.stringify(w));
  expect(w.msgMax, 'message column tier at 1024').toBe('860px');
  expect(w.inputMax, 'composer column tier at 1024').toBe('860px');
  // The boxes fill min(860, available) in their own containers (the
  // transcript scrollbar narrows only .chat-messages).
  const fillsTier = (box: number): boolean => box <= 860 && w.docScroll <= w.inner;
  expect(fillsTier(w.msgBox), 'message box fits the 1024 viewport').toBe(true);
  expect(fillsTier(w.inputBox), 'composer box fits the 1024 viewport').toBe(true);
});

test('375px viewport: column fits, no horizontal overflow', async ({ page }) => {
  await openSeededChat(page);
  await page.setViewportSize({ width: 375, height: 667 });
  const w = await columnWidths(page);
  console.log('375 viewport column widths', JSON.stringify(w));
  expect(w.msgMax, 'message column tier at 375 (base, below 1440)').toBe('860px');
  expect(w.inputMax, 'composer column tier at 375 (base, below 1440)').toBe('860px');
  expect(w.docScroll, 'no horizontal document overflow at 375').toBeLessThanOrEqual(w.inner);
  expect(w.msgBox, 'message box fits the 375 viewport').toBeLessThanOrEqual(w.inner);
  expect(w.inputBox, 'composer box fits the 375 viewport').toBeLessThanOrEqual(w.inner);
});
