import { spawn, type ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import net from 'node:net';
import path from 'node:path';
import { test, expect } from '@playwright/test';
import { pinLocale } from '../../fixtures/lab';
import { repoRoot, runtimeDir } from '../../fixtures/paths';

// Form 1 — the cancelled-run stop notice against a REAL `woca serve`
// binary. Pins the F1 loop end to end: a slow-streaming run stopped
// mid-answer leaves a localized "generation stopped" notice card (the live
// path, e7c9143e), the server persists the derived history ending in
// <run-stopped run="…" reason="user_stop"/> (the persistence seam,
// e5dc795b), and after page.reload() the transcript rebuilds that tag as
// the SAME dim notice card with routed copy — never a raw user bubble, and
// never two cards for one run (the reload path seeds the per-run dedup
// guard, 8e7573bb).
//
// Dedicated instance: this spec spawns its OWN `woca serve` (the binary
// the playwright webServer builds at e2e/.bin/woca, embedded web
// included) with an isolated WOCA_HOME, instead of riding the shared
// personal server. Reason: the case must reconfigure the provider and
// seed a second workspace group, and on the shared server both stomped
// the parallel personal specs — the scripted-model specs race for the one
// provider slot (PUT /api/settings is server-global), and the extra
// sidebar group broke the neighbors' one-group session-button locators.
// A private instance gives this spec its own provider slot, sidebar and
// database (the same isolation the config's webServer applies), with the
// same real binary and the same embedded web build under test.
//
// The model is a scripted OpenAI-compatible SSE stand-in served from the
// test process (the wire shape the activity/hitl specs pin). Runs whose
// first user message carries the STOP-NOTICE marker stream text chunks on
// a 300ms interval (the regression fakemodel's slow-lane pacing) so the
// stop click always lands mid-stream; anything else gets a fast plain
// reply, non-streaming callers plain JSON, and streaming responses always
// end with `data: [DONE]`.
//
// Also pre-paves the F3 wide-screen column work (plan task 6): at a 1024
// viewport the message column's tier is 860px (--chat-col's base tier; the
// >=1440 and >=1800 tiers move in task 6, this one must not). The 280px
// session sidebar leaves ~704px of content width at 1024, so the tier is
// asserted on the computed max-width while the boxes are asserted against
// each container's live available width — min(860, available).

type Msg = { role?: string; content?: string };

const MARKER = 'STOP-NOTICE';
const PROMPT = `${MARKER} stream slowly so the stop click lands mid-answer`;
const CHUNK_MS = 300;
const CHUNKS = 80; // ~24s of stream — far longer than the test needs

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

function plainReply(): string {
  return chunk({ role: 'assistant', content: 'unrelated' }, null) + chunk({}, 'stop') + 'data: [DONE]\n\n';
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

// Runtime hygiene (the posture the playwright webServer applies to
// personal-home): home-*/ws-* dirs from previous runs accrete under
// e2e/.runtime/stopnotice. Entry names embed their creating worker's pid
// (home-<pid>-<ts>), so "owner pid no longer alive" is exactly "left
// over from a previous run". Only those are wiped at startup: sibling
// workers of THIS run have live pids and unique names, so a blanket
// subtree wipe would race them (the config runs fullyParallel across 4
// workers, each spawning its own serve home here).
function wipeStaleRuntimeDirs(): void {
  const dir = path.join(runtimeDir, 'stopnotice');
  fs.mkdirSync(dir, { recursive: true });
  for (const entry of fs.readdirSync(dir)) {
    const pid = Number(/^(?:home|ws)-(\d+)-\d+$/.exec(entry)?.[1]);
    if (!Number.isInteger(pid) || pid <= 0 || pid === process.pid) continue;
    try {
      process.kill(pid, 0); // owner still alive (or pid reused) — keep
      continue;
    } catch {
      // dead owner: leftover from a previous run
    }
    fs.rmSync(path.join(dir, entry), { recursive: true, force: true });
  }
}

/** Spawns the dedicated instance and waits for its /api/health to answer. */
async function startServe(): Promise<{ child: ChildProcess; base: string }> {
  const bin = path.join(repoRoot, 'e2e', '.bin', 'woca');
  if (!fs.existsSync(bin)) {
    throw new Error(`${bin} missing — the playwright webServer builds it before tests run`);
  }
  wipeStaleRuntimeDirs();
  const home = path.join(runtimeDir, 'stopnotice', `home-${process.pid}-${Date.now()}`);
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
  // The scripted model.
  modelServer = http.createServer((req, res) => {
    if (!req.url?.includes('/chat/completions')) {
      res.writeHead(404).end();
      return;
    }
    const parts: Buffer[] = [];
    req.on('data', (b: Buffer) => parts.push(b));
    req.on('end', () => {
      let stream = true;
      let mine = false;
      try {
        const body = JSON.parse(Buffer.concat(parts).toString('utf8')) as { messages?: Msg[]; stream?: boolean };
        stream = body.stream === true; // non-streaming callers omit the flag
        // Route on the FIRST user message: later turns carry recorded
        // history whose text must not re-trigger the slow lane.
        const firstUser = (body.messages ?? []).find((m) => m.role === 'user' || m.role === 'human');
        mine = (firstUser?.content ?? '').includes(MARKER);
      } catch {
        mine = false;
      }
      if (!stream) {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(
          JSON.stringify({
            id: 'chatcmpl-e2e',
            object: 'chat.completion',
            choices: [{ index: 0, message: { role: 'assistant', content: 'Stopped.' }, finish_reason: 'stop' }],
          }),
        );
        return;
      }
      res.writeHead(200, { 'Content-Type': 'text/event-stream' });
      if (!mine) {
        res.end(plainReply());
        return;
      }
      // Slow lane: one chunk every CHUNK_MS. The cancel tears the
      // connection down mid-stream; stop writing once the socket is gone
      // instead of racing EPIPE into the test output. The disconnect is
      // the RESPONSE's 'close' with the response unfinished — the request
      // stream's own 'close' fires as soon as its body is consumed, long
      // before the response is done.
      let i = 0;
      let alive = true;
      const write = (data: string): void => {
        if (!alive || res.destroyed || res.writableEnded) return;
        try {
          res.write(data);
        } catch {
          alive = false;
        }
      };
      const timer = setInterval(() => {
        write(chunk({ content: `slow-stream chunk ${i} ` }, null));
        i++;
        if (i >= CHUNKS) {
          clearInterval(timer);
          write(chunk({}, 'stop'));
          write('data: [DONE]\n\n');
          res.end();
        }
      }, CHUNK_MS);
      res.on('close', () => {
        if (!res.writableEnded) alive = false; // premature: the client is gone
        clearInterval(timer);
      });
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

test('cancelled run: the stop notice card survives page reload', async ({ page }) => {
  test.setTimeout(120_000);

  // The run must carry an explicit workspace (the regression stages' run
  // POST contract): a fresh dir under e2e/.runtime, seeded with one session
  // so the workspace has a sidebar group to create this spec's session in.
  const ws = path.join(runtimeDir, 'stopnotice', `ws-${process.pid}-${Date.now()}`);
  fs.mkdirSync(ws, { recursive: true });
  const seed = await page.request.post(`${base}/api/sessions`, { data: { workspace: ws } });
  expect(seed.status(), 'seed a session in the workspace').toBe(200);
  const seeded = (await seed.json()) as { workspace: string };
  // The server expands/resolves the path — group matching uses its answer.
  const wsPath = seeded.workspace;
  expect(wsPath).toBeTruthy();

  // Permission mode goes to bypass for this spec: the subject is the
  // stop/notice loop, not the approval gates.
  const perms = await page.request.post(`${base}/api/agent/permissions`, {
    data: { mode: 'bypassPermissions' },
  });
  expect(perms.status(), 'set bypass permission mode').toBe(200);

  // Point the dedicated instance at the scripted model.
  const put = await page.request.put(`${base}/api/settings`, {
    data: {
      provider: 'openai',
      model: 'gpt-e2e',
      openai_base_url: `${modelURL}/v1`,
      openai_api_key: 'e2e-key',
    },
  });
  expect(put.status(), 'configure scripted model via settings').toBe(200);

  await pinLocale(page);
  await page.goto(base);
  await expect(page.locator('aside.sidebar')).toBeVisible();

  // A fresh session via the workspace group's own "+" button: its runs
  // carry this workspace path — pinned below on the run POST body.
  const group = page.locator('.ws-group').filter({
    has: page.locator(`.ws-group-name[title="${wsPath}"]`),
  });
  await expect(group).toHaveCount(1);
  const sessionCreated = page.waitForResponse(
    (r) => r.request().method() === 'POST' && /\/api\/sessions$/.test(r.url()),
  );
  await group.getByRole('button', { name: '在此工作区新建会话' }).click();
  expect((await sessionCreated).status()).toBe(200);
  await expect(page.locator('.chat-input textarea')).toBeVisible();

  // Send and capture the run POST: the body must carry the workspace.
  const runPost = page.waitForRequest((r) => r.method() === 'POST' && /\/api\/runs$/.test(r.url()));
  await page.locator('.chat-input textarea').fill(PROMPT);
  await page.keyboard.press('Enter');
  const runReq = await runPost;
  const posted = (await runReq.postDataJSON()) as { session_id: string; prompt: string; workspace: string };
  expect(posted.prompt).toContain(MARKER);
  expect(posted.workspace, 'the run carries the session workspace').toBe(wsPath);
  const sid = posted.session_id;
  expect(sid).toBeTruthy();
  const runBody = (await (await runReq.response())?.json()) as { run_id: string };
  const rid = runBody.run_id;
  expect(rid).toBeTruthy();

  // (1) Mid-stream: the stop button is up and the assistant bubble has
  // received streamed text — the stop click lands inside the model turn.
  const stopBtn = page.locator('.chat-input .send-btn.stop');
  await expect(stopBtn).toBeVisible({ timeout: 20_000 });
  await expect(page.locator('.message.assistant .markdown-body')).toContainText('slow-stream chunk', {
    timeout: 20_000,
  });

  await stopBtn.click();

  // (2) The LIVE notice card: one dim notice titled 系统通知 (chat.notice),
  // collapsed by default, whose expanded body routes to the localized stop
  // copy — never the raw machine tag.
  const noticeCard = page.locator('.w-collapse-card').filter({ hasText: '系统通知' });
  await expect(noticeCard).toHaveCount(1);
  await expect(noticeCard.locator('.tool-output')).toHaveCount(0); // notices render collapsed
  await noticeCard.locator('.w-collapse-head').click();
  await expect(noticeCard.locator('.tool-output')).toContainText('已停止生成');
  await expect(page.locator('.chat-messages')).not.toContainText('run-stopped');
  await expect(stopBtn).toBeHidden();

  // (3) Server truth before reloading: the cancelled round persisted the
  // derived history — this prompt plus THIS run's stop notice tag.
  await expect
    .poll(
      async () => {
        const res = await page.request.get(`${base}/api/sessions/${sid}`);
        const { messages } = (await res.json()) as { messages: Array<{ content?: string }> };
        return messages.some((m) => (m.content ?? '').includes(`<run-stopped run="${rid}" reason="user_stop"/>`));
      },
      { timeout: 30_000 },
    )
    .toBe(true);

  // (4) Reload: the persisted tag must rebuild as the SAME notice card —
  // still notice-kind (collapsed dim card, routed copy, not a user bubble)
  // and exactly one per run (the reload path seeds the dedup guard against
  // a live cancelled-done replay).
  await page.reload();
  await expect(page.locator('aside.sidebar')).toBeVisible();
  // Boot activates the newest session of the whole instance — re-select
  // THIS spec's session explicitly so the assertions are deterministic.
  await group.locator('.session-item').filter({ hasText: MARKER }).click();
  await expect(page.locator('.message.user').first()).toContainText(MARKER);

  const reNotice = page.locator('.w-collapse-card').filter({ hasText: '系统通知' });
  await expect(reNotice).toHaveCount(1);
  await expect(reNotice.locator('.tool-output')).toHaveCount(0);
  await reNotice.locator('.w-collapse-head').click();
  await expect(reNotice.locator('.tool-output')).toContainText('已停止生成');
  // Notice semantics: the machine tag never surfaces as user speech or
  // raw transcript text — one user bubble (the prompt) and nothing else.
  await expect(page.locator('.message.user')).toHaveCount(1);
  await expect(page.locator('.chat-messages')).not.toContainText('run-stopped');

  // (5) 1024-viewport column regression (pre-pave for the F3 tier change):
  // the base tier stays 860px while the >=1440/>=1800 tiers move. The
  // 280px session sidebar leaves ~704px of content at 1024, so the tier is
  // the computed max-width; the boxes fill min(tier, available) in their
  // own containers (the transcript scrollbar narrows only .chat-messages).
  await page.setViewportSize({ width: 1024, height: 768 });
  const widths = await page.evaluate(() => {
    const msg = document.querySelector('.message') as HTMLElement | null;
    const input = document.querySelector('.chat-input-inner') as HTMLElement | null;
    if (!msg || !input) throw new Error('column elements missing');
    const availOf = (el: HTMLElement): number => {
      const parent = el.parentElement as HTMLElement;
      const pcs = getComputedStyle(parent);
      return parent.clientWidth - parseFloat(pcs.paddingLeft) - parseFloat(pcs.paddingRight);
    };
    return {
      msgMax: getComputedStyle(msg).maxWidth,
      inputMax: getComputedStyle(input).maxWidth,
      msgBox: msg.getBoundingClientRect().width,
      inputBox: input.getBoundingClientRect().width,
      msgAvail: availOf(msg),
      inputAvail: availOf(input),
    };
  });
  console.log('1024 viewport column widths', JSON.stringify(widths));
  expect(widths.msgMax, 'message column tier at 1024').toBe('860px');
  expect(widths.inputMax, 'composer column tier at 1024').toBe('860px');
  expect(Math.abs(widths.msgBox - Math.min(860, widths.msgAvail))).toBeLessThanOrEqual(1);
  expect(Math.abs(widths.inputBox - Math.min(860, widths.inputAvail))).toBeLessThanOrEqual(1);
});
