import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { test, expect } from '@playwright/test';
import { pinLocale } from '../../fixtures/lab';
import { startDedicatedServe, stopDedicatedServe, type DedicatedServe } from '../../fixtures/dedicated';
import { runtimeDir } from '../../fixtures/paths';

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
// same real binary and the same embedded web build under test. The spawn
// harness itself lives in fixtures/dedicated.ts (shared with the other
// provider-mutating specs).
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

type Msg = { role?: string; content?: string; tool_call_id?: string; tool_calls?: Array<{ id?: string }> };

const MARKER = 'STOP-NOTICE';
const PROMPT = `${MARKER} stream slowly so the stop click lands mid-answer`;
const CHUNK_MS = 300;
const CHUNKS = 80; // ~24s of stream — far longer than the test needs

// Tool lane (spec 2026-11-19-cancel-partial-output §4): a run whose prompt
// carries the tool marker streams a little text, then calls Bash sleep 30 —
// the stop click lands while the tool executes. The mid-tool window is a
// genuine engine race (the fork's stream emitter drops the tools batch on a
// cancelled ctx with select-random priority): the call closes EITHER with
// the server's synthesized abort result OR with the killed shell's own
// "Exit code -1." output. Both outcomes are legal, both close the card, and
// the next round's model request must never carry a dangling tool_use — the
// assertions below pin exactly that disjunction (the synthesized copy
// verbatim is pinned deterministically by the Go suite's parked-approval
// window, where no race exists).
const TOOL_MARKER = 'STOP-TOOL';
const TOOL_PROMPT = `${TOOL_MARKER} inspect the workspace with a slow command`;
const TOOL_CALL_ID = 'call-stop-tool-1';
const TOOL_RESULT_RE = /Tool call aborted by user after |^Exit code -1\./;

// Every chat-completions body the scripted model received, newest last —
// the next-round assertion reads the exact history the server replayed.
const modelRequests: Msg[][] = [];

let modelURL = '';
let modelServer: http.Server | undefined;
let serve: DedicatedServe | undefined;
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

/** The tool lane's opening move: partial text, then one real Bash call. */
function toolLaneReply(): string {
  return (
    chunk({ role: 'assistant' }, null) +
    chunk({ content: 'checking the workspace first ' }, null) +
    chunk(
      {
        tool_calls: [
          {
            index: 0,
            id: TOOL_CALL_ID,
            type: 'function',
            function: { name: 'Bash', arguments: JSON.stringify({ command: 'sleep 30' }) },
          },
        ],
      },
      null,
    ) +
    chunk({}, 'tool_calls') +
    'data: [DONE]\n\n'
  );
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
      let toolLane = false;
      try {
        const body = JSON.parse(Buffer.concat(parts).toString('utf8')) as { messages?: Msg[]; stream?: boolean };
        stream = body.stream === true; // non-streaming callers omit the flag
        const msgs = body.messages ?? [];
        modelRequests.push(msgs);
        // Route on the FIRST user message: later turns carry recorded
        // history whose text must not re-trigger the slow lane.
        const firstUser = msgs.find((m) => m.role === 'user' || m.role === 'human');
        mine = (firstUser?.content ?? '').includes(MARKER);
        // Tool lane: this conversation's opening turn, and the scripted
        // call is still unanswered (once history carries its result — real
        // or synthesized — the conversation moves to plain replies).
        toolLane =
          msgs.some((m) => (m.content ?? '').includes(TOOL_MARKER)) &&
          !msgs.some((m) => m.role === 'tool' && m.tool_call_id === TOOL_CALL_ID);
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
      if (toolLane) {
        res.end(toolLaneReply());
        return;
      }
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
  serve = await startDedicatedServe('stopnotice');
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

// Form 1b — the cancelled TOOL run (spec 2026-11-19-cancel-partial-output).
// A run whose prompt carries the tool marker streams partial text, calls
// Bash sleep 30 (real execution under bypassPermissions), and the stop click
// lands while the tool runs. Pins: (1) the cancelling tab's open tool card
// flips to the local aborted terminal (no spinner survives the stop);
// (2) after reload the partial assistant bubble, the closed tool card
// (synthesized abort result or the killed shell's own output — the engine
// race both ways, see the lane comment above) and exactly one notice card;
// (3) the persisted transcript closes the call server-side; (4) the next
// round's model request carries the closed call and NEVER a dangling
// tool_use, plus the model-visible stop tag.
test('cancelled tool run: the partial transcript and the closed call survive reload', async ({ page }) => {
  test.setTimeout(120_000);

  const ws = path.join(runtimeDir, 'stopnotice', `ws-tool-${process.pid}-${Date.now()}`);
  fs.mkdirSync(ws, { recursive: true });
  const seed = await page.request.post(`${base}/api/sessions`, { data: { workspace: ws } });
  expect(seed.status(), 'seed a session in the tool workspace').toBe(200);
  const wsPath = ((await seed.json()) as { workspace: string }).workspace;
  expect(wsPath).toBeTruthy();

  const perms = await page.request.post(`${base}/api/agent/permissions`, {
    data: { mode: 'bypassPermissions' },
  });
  expect(perms.status(), 'set bypass permission mode').toBe(200);
  const put = await page.request.put(`${base}/api/settings`, {
    data: {
      provider: 'openai',
      model: 'gpt-e2e',
      openai_base_url: `${modelURL}/v1`,
      openai_api_key: 'e2e-key',
    },
  });
  expect(put.status(), 'point the instance at the scripted model').toBe(200);

  await pinLocale(page);
  await page.goto(base);
  await expect(page.locator('aside.sidebar')).toBeVisible();

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

  const runPost = page.waitForRequest((r) => r.method() === 'POST' && /\/api\/runs$/.test(r.url()));
  await page.locator('.chat-input textarea').fill(TOOL_PROMPT);
  await page.keyboard.press('Enter');
  const runReq = await runPost;
  const posted = (await runReq.postDataJSON()) as { session_id: string; prompt: string };
  expect(posted.prompt).toContain(TOOL_MARKER);
  const sid = posted.session_id;
  const runBody = (await (await runReq.response())?.json()) as { run_id: string };
  const rid = runBody.run_id;
  expect(rid).toBeTruthy();

  // (1) The tool card opens with its spinner while the partial text streams;
  // the stop click lands inside the tool execution window.
  const card = page.locator('.stream-card').filter({ hasText: 'Bash' }).first();
  await expect(card.locator('.tool-status-running')).toHaveCount(1, { timeout: 20_000 });
  await expect(page.locator('.message.assistant .markdown-body')).toContainText('checking the workspace first', {
    timeout: 20_000,
  });
  const stopBtn = page.locator('.chat-input .send-btn.stop');
  await expect(stopBtn).toBeVisible();
  await stopBtn.click();

  // The cancelling tab aborted its stream before the server's synthesized
  // tool_result events could arrive: cancel() flips the open card to the
  // local aborted terminal and the spinner is gone. Tool cards start
  // collapsed (autoCollapseTools) — expand to see the flipped output.
  await expect(card.locator('.tool-status-running')).toHaveCount(0, { timeout: 10_000 });
  await card.locator('.w-collapse-head').click();
  await expect(card.locator('.tool-output')).toContainText('已中止');

  // (3) Server truth: the persisted transcript closes the call — the
  // synthesized abort result or the killed shell's own output.
  let closerContent = '';
  await expect
    .poll(
      async () => {
        const res = await page.request.get(`${base}/api/sessions/${sid}`);
        const { messages } = (await res.json()) as { messages: Array<{ role?: string; tool_call_id?: string; content?: string }> };
        const tool = messages.find((m) => m.role === 'tool' && m.tool_call_id === TOOL_CALL_ID);
        closerContent = tool?.content ?? '';
        return TOOL_RESULT_RE.test(closerContent);
      },
      { timeout: 30_000 },
    )
    .toBe(true);
  // The stop tag closes the round after the closed call.
  const res0 = await page.request.get(`${base}/api/sessions/${sid}`);
  const msgs0 = ((await res0.json()) as { messages: Array<{ content?: string }> }).messages;
  expect(msgs0.some((m) => (m.content ?? '').includes(`<run-stopped run="${rid}" reason="user_stop"/>`))).toBe(true);

  // (2) Reload: the partial bubble, the closed card, one notice card.
  await page.reload();
  await expect(page.locator('aside.sidebar')).toBeVisible();
  await group.locator('.session-item').filter({ hasText: TOOL_MARKER }).click();
  await expect(page.locator('.message.assistant .markdown-body')).toContainText('checking the workspace first');
  const reCard = page.locator('.stream-card').filter({ hasText: 'Bash' }).first();
  await expect(reCard.locator('.tool-status-running')).toHaveCount(0);
  await reCard.locator('.w-collapse-head').click();
  await expect(reCard.locator('.tool-output')).toContainText(TOOL_RESULT_RE);
  const noticeCard = page.locator('.w-collapse-card').filter({ hasText: '系统通知' });
  await expect(noticeCard).toHaveCount(1);

  // (4) The next round proves model visibility: the server replays the
  // history with the closed call — never a dangling tool_use — and the stop
  // tag rides along as machine-visible history. The matching request must
  // carry the closed call (the session-title summarizer sees the new prompt
  // too, but without the tool history).
  await page.locator('.chat-input textarea').fill('and now just wrap up');
  await page.keyboard.press('Enter');
  const isReplay = (messages: Msg[]): boolean =>
    messages.some((m) => (m.content ?? '').includes('and now just wrap up')) &&
    messages.some((m) => m.role === 'tool' && m.tool_call_id === TOOL_CALL_ID);
  await expect
    .poll(
      () => modelRequests.some(isReplay),
      { timeout: 30_000 },
    )
    .toBe(true);
  const replayed = modelRequests.findLast(isReplay)!;
  const answered = new Set(replayed.filter((m) => m.role === 'tool').map((m) => m.tool_call_id));
  const dangling: string[] = [];
  for (const m of replayed) {
    for (const tc of m.tool_calls ?? []) {
      if (tc.id && !answered.has(tc.id)) dangling.push(tc.id);
    }
  }
  expect(dangling, 'no tool_use may reach the model without its result').toEqual([]);
  const toolMsg = replayed.find((m) => m.role === 'tool' && m.tool_call_id === TOOL_CALL_ID);
  expect(toolMsg?.content ?? '', 'the closed call rides along with terminal content').toMatch(TOOL_RESULT_RE);
  expect(replayed.some((m) => (m.content ?? '').includes(`<run-stopped run="${rid}"`))).toBe(true);
});
