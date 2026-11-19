import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { test, expect, type Page } from '@playwright/test';
import { pinLocale } from '../../fixtures/lab';
import { startDedicatedServe, stopDedicatedServe, type DedicatedServe } from '../../fixtures/dedicated';
import { runtimeDir } from '../../fixtures/paths';

// Form 1 — concurrent dual-session dual-stream isolation against a REAL
// `woca serve` binary. The parked cross-contamination report (bug 7, wave1
// review) could not be reproduced server-side (internal/server/
// run_deltarace_test.go: 8 concurrent runs, -race, both storage forms).
// The two remaining suspects were client-side: the web stream store's sid
// closures (web/src/stores/sessions/stream.ts) and the tunnel forwarding
// path. This spec drives the FIRST suspect at full stack: two browser
// contexts, each owning one session of the same workspace, both runs
// streaming marker-tagged deltas at the same time — plus a mid-stream
// sidebar switch on one side (the activeId churn the closure suspicion was
// about: deltas must keep landing on their own session's items, never the
// visible one).
//
// The scripted model routes on the FIRST user message: MARK-ALPHA streams
// "ALPHA-delta-N" chunks, MARK-BETA streams "BETA-delta-N" (both slow, so
// the two streams genuinely overlap); non-streaming callers get plain JSON
// and streaming responses end with `data: [DONE]`.
//
// Verdict carrier: if this spec passes (repeatably), the web closure
// suspect is excluded alongside the server-side one — the remaining
// explanation for the original report is staleness (the wave1 fixes), and
// the case closes as such.

type Msg = { role?: string; content?: string };

const CHUNK_MS = 200;
const CHUNKS = 80; // ~16s of stream — both runs overlap and outlive the reload+switch steps

let modelURL = '';
let modelServer: http.Server | undefined;
let serve: DedicatedServe | undefined;
let base = '';

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

test.beforeAll(async () => {
  modelServer = http.createServer((req, res) => {
    if (!req.url?.includes('/chat/completions')) {
      res.writeHead(404).end();
      return;
    }
    const parts: Buffer[] = [];
    req.on('data', (b: Buffer) => parts.push(b));
    req.on('end', () => {
      let stream = true;
      let marker = '';
      try {
        const body = JSON.parse(Buffer.concat(parts).toString('utf8')) as { messages?: Msg[]; stream?: boolean };
        stream = body.stream === true; // non-streaming callers omit the flag
        const firstUser = (body.messages ?? []).find((m) => m.role === 'user' || m.role === 'human');
        const text = firstUser?.content ?? '';
        marker = text.includes('MARK-ALPHA') ? 'ALPHA' : text.includes('MARK-BETA') ? 'BETA' : '';
      } catch {
        marker = '';
      }
      if (!stream) {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(
          JSON.stringify({
            id: 'chatcmpl-e2e',
            object: 'chat.completion',
            choices: [
              { index: 0, message: { role: 'assistant', content: marker || 'unrelated' }, finish_reason: 'stop' },
            ],
          }),
        );
        return;
      }
      res.writeHead(200, { 'Content-Type': 'text/event-stream' });
      if (!marker) {
        res.end(plainReply());
        return;
      }
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
        write(chunk({ content: `${marker}-delta-${i} ` }, null));
        i++;
        if (i >= CHUNKS) {
          clearInterval(timer);
          write(chunk({}, 'stop'));
          write('data: [DONE]\n\n');
          res.end();
        }
      }, CHUNK_MS);
      res.on('close', () => {
        if (!res.writableEnded) alive = false;
        clearInterval(timer);
      });
    });
  });
  await new Promise<void>((resolve) => modelServer!.listen(0, '127.0.0.1', resolve));
  const addr = modelServer!.address();
  if (!addr || typeof addr !== 'object') throw new Error('fake model has no address');
  modelURL = `http://127.0.0.1:${addr.port}`;

  serve = await startDedicatedServe('deltarace');
  base = serve.base;
});

test.afterAll(async () => {
  if (serve) {
    await stopDedicatedServe(serve);
  }
  if (modelServer) {
    modelServer.closeAllConnections?.();
    await new Promise<void>((r) => modelServer!.close(() => r()));
  }
});

/** Owns one session end to end: creates it via the group "+", sends the
 * marker prompt, returns {sid, runId}. Must await the session-create
 * response BEFORE sending (the store switches sessions on it). */
async function ownSession(page: Page, markerPrompt: string): Promise<{ sid: string; runId: string }> {
  const group = page.locator('.ws-group').first();
  const sessionCreated = page.waitForResponse(
    (r) => r.request().method() === 'POST' && /\/api\/sessions$/.test(r.url()),
  );
  await group.getByRole('button', { name: '在此工作区新建会话' }).click();
  const created = await sessionCreated;
  expect(created.status()).toBe(200);
  const sid = ((await created.json()) as { session_id: string }).session_id;
  expect(sid).toBeTruthy();

  const runPost = page.waitForRequest((r) => r.method() === 'POST' && /\/api\/runs$/.test(r.url()));
  await page.locator('.chat-input textarea').fill(markerPrompt);
  await page.keyboard.press('Enter');
  const req = await runPost;
  const body = (await req.postDataJSON()) as { session_id: string; prompt: string };
  expect(body.session_id).toBe(sid);
  expect(body.prompt).toContain(markerPrompt.split(' ')[0]);
  const runBody = (await (await req.response())?.json()) as { run_id: string };
  expect(runBody.run_id).toBeTruthy();
  return { sid, runId: runBody.run_id };
}

test('two concurrent sessions stream their own deltas only', async ({ browser }) => {
  test.setTimeout(180_000);

  // Seed the workspace (and its sidebar group) with one session.
  const ws = path.join(runtimeDir, 'deltarace', `ws-${process.pid}-${Date.now()}`);
  fs.mkdirSync(ws, { recursive: true });
  const ctxA = await browser.newContext();
  const pageA = await ctxA.newPage();
  const seed = await pageA.request.post(`${base}/api/sessions`, { data: { workspace: ws } });
  expect(seed.status()).toBe(200);

  const perms = await pageA.request.post(`${base}/api/agent/permissions`, {
    data: { mode: 'bypassPermissions' },
  });
  expect(perms.status()).toBe(200);
  const put = await pageA.request.put(`${base}/api/settings`, {
    data: {
      provider: 'openai',
      model: 'gpt-e2e',
      openai_base_url: `${modelURL}/v1`,
      openai_api_key: 'e2e-key',
    },
  });
  expect(put.status()).toBe(200);

  // Two separate contexts: independent storage, so each tab keeps its own
  // active session without cross-tab sync fighting over the preference.
  const ctxB = await browser.newContext();
  const pageB = await ctxB.newPage();
  for (const p of [pageA, pageB]) {
    await pinLocale(p);
    await p.goto(base);
    await expect(p.locator('.chat-input textarea')).toBeVisible();
  }

  const a = await ownSession(pageA, 'MARK-ALPHA please answer');
  const b = await ownSession(pageB, 'MARK-BETA please answer');
  expect(a.sid).not.toBe(b.sid);

  // (1) Genuine overlap: both stops are up at the same time, both bubbles
  // are streaming their own marker.
  for (const [p, marker] of [
    [pageA, 'ALPHA'],
    [pageB, 'BETA'],
  ] as const) {
    await expect(p.locator('.chat-input .send-btn.stop')).toBeVisible({ timeout: 30_000 });
    await expect(p.locator('.message.assistant .markdown-body')).toContainText(`${marker}-delta-`, {
      timeout: 30_000,
    });
  }
  await expect(pageA.locator('.chat-input .send-btn.stop')).toBeVisible();
  await expect(pageB.locator('.chat-input .send-btn.stop')).toBeVisible();

  // (2) Mid-stream activeId churn on page A: reload (its session list only
  // refreshes on load — page A never saw B's session; the reload ALSO
  // exercises the mid-run reattach path, where replayed events rebuild the
  // items), reselect A's session (no persisted active preference — the
  // reload lands on the first session), then switch to B's session and
  // back. B's view (now live on page A too) must carry only BETA text; the
  // switch must never steer ALPHA deltas into the visible session.
  await pageA.reload();
  await expect(pageA.locator('.chat-input textarea')).toBeVisible();
  const groupA = pageA.locator('.ws-group').first();
  await groupA.locator(`[data-testid="session-item"][data-sid="${a.sid}"]`).click();
  await expect(pageA.locator('.message.assistant .markdown-body')).toContainText('ALPHA-delta-', {
    timeout: 30_000, // the reattach replay rebuilds the streaming bubble
  });
  await groupA.locator(`[data-testid="session-item"][data-sid="${b.sid}"]`).click();
  await expect(pageA.locator('.message.assistant .markdown-body')).toContainText('BETA-delta-', {
    timeout: 30_000,
  });
  await expect(pageA.locator('.chat-messages')).not.toContainText('ALPHA-delta-');
  await groupA.locator(`[data-testid="session-item"][data-sid="${a.sid}"]`).click();
  await expect(pageA.locator('.message.assistant .markdown-body')).toContainText('ALPHA-delta-', {
    timeout: 30_000,
  });

  // (3) Both runs settle: each page's transcript holds its own tail chunk
  // and never a character of the other marker.
  for (const [p, mine, other] of [
    [pageA, 'ALPHA', 'BETA'],
    [pageB, 'BETA', 'ALPHA'],
  ] as const) {
    await expect(p.locator('.chat-messages')).toContainText(`${mine}-delta-${CHUNKS - 1}`, {
      timeout: 60_000,
    });
    await expect(p.locator('.chat-messages')).not.toContainText(`${other}-delta-`);
  }

  // (4) Server truth: each session's persisted transcript mirrors the same
  // isolation (the store must not have corrupted what the server saved).
  for (const [sid, mine, other] of [
    [a.sid, 'ALPHA', 'BETA'],
    [b.sid, 'BETA', 'ALPHA'],
  ] as const) {
    const res = await pageA.request.get(`${base}/api/sessions/${sid}`);
    expect(res.status()).toBe(200);
    const { messages } = (await res.json()) as { messages: Array<{ content?: string }> };
    const joined = messages.map((m) => m.content ?? '').join('\n');
    expect(joined, `session ${sid} keeps its own text`).toContain(`${mine}-delta-${CHUNKS - 1}`);
    expect(joined, `session ${sid} must not contain ${other} text`).not.toContain(`${other}-delta-`);
  }

  await ctxA.close();
  await ctxB.close();
});
