import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { test, expect, type Page } from '@playwright/test';
import { pinLocale } from '../../fixtures/lab';
import { startDedicatedServe, stopDedicatedServe, type DedicatedServe } from '../../fixtures/dedicated';
import { runtimeDir } from '../../fixtures/paths';

// Form 1 — graph-native HITL against the REAL `woca serve` binary. The
// model is a scripted OpenAI-compatible SSE stand-in served from the test
// process (the same wire shape the activity spec pins). The scripted
// conversation fans TWO foreground Tasks out; each child's model call is
// scripted by its prompt marker (FILE-A / FILE-B) so both children pause on
// a gated call in ONE superstep — the multi-card approval stack, session
// grants, the deny path, and the Subagents tab are asserted end-to-end.
//
// Dedicated instance (the stopnotice/chatcolumn pattern): this spec
// reconfigures the provider AND the permission mode, both server-global on
// `woca serve` — the settings `provider` key is a single live selector with
// no per-run override, so on the shared personal webServer the activity
// spec's identical setup raced this one for the slot (runs landing on the
// wrong scripted model or on a dead endpoint after the other spec's
// teardown reset; reproduced at HEAD with activity+hitl --repeat-each=5:
// 8/10 failed). A private instance gives this spec its own provider slot,
// permission mode, sidebar and database — the same real binary and
// embedded web build under test.

type Msg = { role: string; content?: string; tool_calls?: unknown[] };

let modelURL = '';
let modelRequests = 0;
let modelServer: http.Server | undefined;
let serve: DedicatedServe | undefined;
let base = '';
/** The seeded throwaway workspace (server-resolved path) — see beforeAll. */
let wsGroupPath = '';

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

function sse(res: http.ServerResponse, deltas: Record<string, unknown>[], finish: string | null) {
  res.writeHead(200, { 'content-type': 'text/event-stream' });
  for (const d of deltas) res.write(chunk(d, null));
  if (finish) res.write(chunk({}, finish));
  res.write('data: [DONE]\n\n');
  res.end();
}

function toolCall(id: string, name: string, args: Record<string, unknown>, index = 0) {
  return {
    index,
    id,
    type: 'function',
    function: { name, arguments: JSON.stringify(args) },
  };
}

/** The machine-tag notice prefixes woca's own consumers exclude when they
 * look for a run's real request (internal/machinetag Prefixes): mid-run
 * injections like <relevant-memory> rides as USER messages AFTER the turn's
 * tool results, so a naive "last user message" turn boundary misclassifies
 * the round as unanswered and the script would re-issue a gated call that
 * already ran (the phantom re-ask behind this spec's former wedge). */
const MACHINE_TAG_PREFIXES = ['<relevant-memory>', '<queued-user-input', '<stop-hook>', '<run-stopped', '<task-fork>'];

function isMachineTagNotice(m: Msg): boolean {
  if (m.role !== 'user') return false;
  const c = m.content ?? '';
  return MACHINE_TAG_PREFIXES.some((p) => c.startsWith(p));
}

/** The scripted model: parent turns are identified by the HITL-FANOUT
 * marker, child turns by FILE-A / FILE-B in the transcript. Children gated
 * call: Bash `npm install ...` (classifier non-readonly, grantable prefix)
 * for the grant scenario; Write for the plain allow/deny scenario. */
function scriptFor(messages: Msg[]): { deltas: Record<string, unknown>[]; finish: string } {
  const text = messages.map((m) => m.content ?? '').join('\n');
  // Tool results OF THE CURRENT TURN: anything after the last REAL user
  // message (machine-tag notices excluded — see MACHINE_TAG_PREFIXES). A
  // second fan-out in the same session must re-emit the Task batch even
  // though the transcript already carries the first turn's tool results.
  const turn = messages.filter((m) => !isMachineTagNotice(m));
  const lastHuman = turn.map((m) => m.role).lastIndexOf('user');
  const hasToolResults = turn.slice(lastHuman + 1).some((m) => m.role === 'tool');
  if (text.includes('FILE-B')) {
    if (hasToolResults) return { deltas: [{ content: 'child B done' }], finish: 'stop' };
    return {
      deltas: [{ role: 'assistant', tool_calls: [toolCall('cb1', 'Bash', { command: 'npm install left-pad' })] }],
      finish: 'tool_calls',
    };
  }
  if (text.includes('FILE-A')) {
    if (hasToolResults) return { deltas: [{ content: 'child A done' }], finish: 'stop' };
    return {
      deltas: [{ role: 'assistant', tool_calls: [toolCall('ca1', 'Bash', { command: 'npm install left-pad' })] }],
      finish: 'tool_calls',
    };
  }
  if (text.includes('HITL-FANOUT')) {
    if (hasToolResults) return { deltas: [{ content: 'Fan-out complete.' }], finish: 'stop' };
    return {
      deltas: [
        {
          role: 'assistant',
          tool_calls: [
            toolCall('tA', 'Task', { description: 'install A', prompt: 'FILE-A install the package' }, 0),
            toolCall('tB', 'Task', { description: 'install B', prompt: 'FILE-B install the package' }, 1),
          ],
        },
      ],
      finish: 'tool_calls',
    };
  }
  return { deltas: [{ content: 'unrelated' }], finish: 'stop' };
}

test.beforeAll(async () => {
  modelServer = http.createServer((req, res) => {
    if (!req.url?.includes('/chat/completions')) {
      res.writeHead(404).end();
      return;
    }
    const body: Uint8Array[] = [];
    req.on('data', (d) => body.push(d));
    req.on('end', () => {
      modelRequests++;
      const parsed = JSON.parse(Buffer.concat(body).toString()) as { messages: Msg[] };
      const script = scriptFor(parsed.messages ?? []);
      sse(res, script.deltas, script.finish);
    });
  });
  await new Promise<void>((r) => modelServer!.listen(0, '127.0.0.1', r));
  const addr = modelServer.address() as { port: number };
  modelURL = `http://127.0.0.1:${addr.port}`;

  // The dedicated personal instance (fixtures/dedicated.ts): own provider
  // slot and permission mode, so no teardown reset is needed — the instance
  // dies with the worker and never strands a neighbor on a dead provider.
  serve = await startDedicatedServe('hitl');
  base = serve.base;

  // Seed a throwaway workspace (the stopnotice posture): its sidebar group
  // hosts this spec's sessions, AND the session-granted `npm install` the
  // script issues executes inside it — a dedicated spawn's default
  // workspace would be the worker's cwd (the e2e/ tree), while the shared
  // server ran it in the repo root. The dir carries its own minimal
  // package.json so npm roots INSIDE it: npm resolves the install root by
  // walking up to the nearest package.json, and e2e/ has one.
  const ws = path.join(runtimeDir, 'hitl', `ws-${process.pid}-${Date.now()}`);
  fs.mkdirSync(ws, { recursive: true });
  fs.writeFileSync(
    path.join(ws, 'package.json'),
    JSON.stringify({ name: 'hitl-e2e-workspace', private: true }, null, 2) + '\n',
  );
  const seed = await fetch(`${base}/api/sessions`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ workspace: ws }),
  });
  if (!seed.ok) throw new Error(`workspace seed failed: ${seed.status}`);
  const seeded = (await seed.json()) as { workspace: string };
  wsGroupPath = seeded.workspace; // the server resolves/expands the path
  if (!wsGroupPath) throw new Error('workspace seed response incomplete');
});

test.afterAll(async () => {
  if (serve) {
    await stopDedicatedServe(serve);
  }
  if (modelServer) {
    await new Promise<void>((r) => modelServer!.close(() => r()));
  }
});

async function freshSession(page: Page): Promise<string> {
  // The seeded workspace's own "+" button: its sessions carry the throwaway
  // workspace, so the granted npm install executes there (see beforeAll).
  const group = page.locator('.ws-group').filter({
    has: page.locator(`.ws-group-name[title="${wsGroupPath}"]`),
  });
  const sessionCreated = page.waitForResponse(
    (r) => r.request().method() === 'POST' && /\/api\/sessions$/.test(r.url()),
  );
  await group.getByRole('button', { name: '在此工作区新建会话' }).click();
  expect((await sessionCreated).status()).toBe(200);
  await expect(page.locator('.chat-input textarea')).toBeVisible();
  const runPost = page.waitForRequest((r) => r.method() === 'POST' && /\/api\/runs$/.test(r.url()));
  await page.locator('.chat-input textarea').fill('HITL-FANOUT run the installs');
  await page.keyboard.press('Enter');
  const posted = (await (await runPost).postDataJSON()) as { session_id: string };
  const runRes = await (await runPost).response();
  const runBody = runRes ? ((await runRes.json()) as { run_id?: string }) : {};
  ;(page as unknown as { _lastRunId?: string })._lastRunId = runBody.run_id ?? '';
  const sid = posted.session_id;
  expect(sid).toBeTruthy();
  return sid;
}

async function configure(page: Page, mode: string) {
  const put = await page.request.put(`${base}/api/settings`, {
    data: {
      provider: 'openai',
      model: 'gpt-e2e',
      openai_base_url: `${modelURL}/v1`,
      openai_api_key: 'e2e-key',
    },
  });
  expect(put.status(), 'configure scripted model via settings').toBe(200);
  const perms = await page.request.post(`${base}/api/agent/permissions`, { data: { mode } });
  expect(perms.status(), `set ${mode} permission mode`).toBe(200);
}

async function sessionDone(page: Page, sid: string, marker: string) {
  await expect
    .poll(
      async () => {
        const res = await page.request.get(`${base}/api/sessions/${sid}`);
        const { messages } = (await res.json()) as { messages: Array<{ content?: string }> };
        return messages.some((m) => (m.content ?? '').includes(marker));
      },
      { timeout: 45_000 },
    )
    .toBe(true);
}

test('hitl: two parallel child approvals, session grant, deny, subagents tab', async ({ page }) => {
  test.setTimeout(240_000);
  await configure(page, 'default');
  await pinLocale(page);
  await page.goto(base);
  await expect(page.locator('aside.sidebar')).toBeVisible();
  const sid = await freshSession(page);

  // (1) Both children pause in one superstep: the stack renders TWO cards,
  // each a Bash npm-install ask from its own child.
  const stack = page.locator('[data-testid="hitl-stack"]');
  await expect(stack).toBeVisible({ timeout: 30_000 });
  const cards = stack.locator('[data-testid^="hitl-card-"]');
  await expect(cards).toHaveCount(2);

  // (2) Session grant on the FIRST card: the rule-key preview is visible
  // before answering; granting collapses the SIBLING card too (same
  // bash:npm * prefix — the second ask must not re-prompt).
  const first = cards.nth(0);
  const firstId = await first.getAttribute('data-testid');
  const second = cards.nth(1);
  const secondId = await second.getAttribute('data-testid');
  expect(firstId).not.toBe(secondId);
  await expect(first.locator(`[data-testid="${firstId!.replace('hitl-card-', 'hitl-rule-key-')}"]`)).toBeVisible();
  const grantPost = page.waitForResponse((r) => /\/answer$/.test(r.url()));
  await first.locator(`[data-testid="${firstId!.replace('hitl-card-', 'hitl-grant-session-')}"]`).click();
  const gres = await grantPost;
  console.log('GRANT STATUS', gres.status(), (await gres.text()).slice(0, 120), gres.url());
  // The granted card resolves; the sibling's ask hits the fresh session
  // grant and executes without another card (the stack closes).
  await expect(stack).toBeHidden({ timeout: 20_000 });

  // (3) The run completes: both children ran their install and reported.
  await sessionDone(page, sid, 'Fan-out complete.');

  // (4) Grants management (server truth; the panel UI is unit-tested): the
  // session carries the npm rule; revoking clears it (the next fan-out asks
  // again).
  const grantsResp = await page.request.get(`${base}/api/agent/grants?session=${sid}`);
  expect(grantsResp.status()).toBe(200);
  const grantsBody = (await grantsResp.json()) as { grants: Array<{ key: string }> };
  expect(grantsBody.grants.some((g) => g.key.includes('npm install'))).toBe(true);
  const del = await page.request.delete(
    `${base}/api/agent/grants?session=${sid}&key=${encodeURIComponent(grantsBody.grants[0].key)}`,
  );
  expect(del.status()).toBe(200);
  const after = (await (await page.request.get(`${base}/api/agent/grants?session=${sid}`)).json()) as {
    grants: Array<{ key: string }>;
  };
  expect(after.grants).toHaveLength(0);

  // (5) DENY PATH in a fresh session: allow one child; the sibling card
  // stays optimistically answerable while the server re-parks it — deny
  // inside that resume window must BUFFER server-side and apply to the
  // re-park (the former "no pending interrupt" race), the denied child
  // reports, and the run completes.
  const sid2 = await freshSession(page);
  const cards2 = stack.locator('[data-testid^="hitl-card-"]');
  await expect(stack).toBeVisible({ timeout: 30_000 });
  await expect(cards2).toHaveCount(2);
  const allow = cards2.nth(0);
  const allowId = (await allow.getAttribute('data-testid'))!.replace('hitl-card-', '');
  await allow.locator(`[data-testid="hitl-allow-${allowId}"]`).click();
  const cardsAfterAllow = stack.locator('[data-testid^="hitl-card-"]');
  await expect(cardsAfterAllow).toHaveCount(1, { timeout: 20_000 });
  const deny = cardsAfterAllow.nth(0);
  const denyId = (await deny.getAttribute('data-testid'))!.replace('hitl-card-', '');
  await deny.locator(`[data-testid="hitl-deny-${denyId}"]`).click();
  await expect(stack).toBeHidden({ timeout: 20_000 });
  await sessionDone(page, sid2, 'Fan-out complete.');


  // (6) The Subagents tab: rows for the four child lifetimes, a terminal
  // status, and the readonly transcript expand.
  await page.locator('.act-toggle').click();
  const card = page.locator('.act-pop-card');
  await expect(card).toBeVisible();
  await card.locator('[data-testid="sub-tab"]').click();
  const rows = card.locator('[data-testid^="sub-row-"]');
  await expect(rows.first()).toBeVisible({ timeout: 15_000 });
  await expect
    .poll(async () => rows.count(), { timeout: 15_000 })
    .toBeGreaterThanOrEqual(2);
  const doneRow = rows.filter({ has: page.locator('[data-testid^="sub-status-"]', { hasText: /完成|done/ }) }).first();
  await expect(doneRow).toBeVisible({ timeout: 15_000 });
  await doneRow.locator('[data-testid^="sub-status-"]').click();
  // The reveal container is the row's sibling (outside SubagentRow's root).
  const transcript = doneRow.locator('xpath=following-sibling::*[1]').locator('[data-testid^="sub-transcript-"]:not([data-testid^="sub-transcript-copy"])');
  await expect(transcript).toBeVisible({ timeout: 10_000 });

  // (7) Background tab initiator column (soft): this session's installs ran
  // in the foreground, so the tab may hold no rows — the column, attribution
  // and filter are pinned by vitest; here we only smoke the tab switch.
  await card.locator('[data-tab="background"]').click();
  await expect(card.locator('[data-tab="background"]')).toHaveAttribute('aria-selected', 'true');
});
