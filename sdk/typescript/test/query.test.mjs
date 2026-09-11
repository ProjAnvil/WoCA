import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { query } from "../dist/index.js";

const fake = path.join(path.dirname(fileURLToPath(import.meta.url)), "fake-woca.sh");

test("streams events and resolves result", async () => {
  const q = query({ prompt: "hi", options: { pathToWoca: fake, model: "m", permissionMode: "bypassPermissions", maxTurns: 3 } });
  const types = [];
  for await (const ev of q) types.push(ev.type);
  assert.deepEqual(types, ["assistant", "assistant", "user", "result"]);
  const res = await q.result;
  assert.equal(res.subtype, "success");
  assert.equal(res.usage.input_tokens, 10);
});

test("maps options to CLI flags", async () => {
  const q = query({ prompt: "hi", options: { pathToWoca: fake, model: "m", maxTurns: 3, permissionMode: "bypassPermissions", denyCommands: ["rm -rf"], db: "none" } });
  let first;
  for await (const ev of q) { first = first ?? ev; }
  const echoed = first.message.content;
  for (const f of ["--model m", "--max-turns 3", "--permission-mode bypassPermissions", "--deny-command rm -rf", "--db none", "--output-format stream-json"])
    assert.ok(echoed.includes(f), `missing flag: ${f} in ${echoed}`);
});

test("non-zero exit without result rejects", async () => {
  const q = query({ prompt: "hi", options: { pathToWoca: fake } });
  process.env.FAKE_WOCA_FAIL = "1";
  await assert.rejects(async () => { for await (const _ of q) {} }, /exit code 1/);
  delete process.env.FAKE_WOCA_FAIL;
});

test("cancel terminates the process", async () => {
  const q = query({ prompt: "hi", options: { pathToWoca: fake } });
  q.cancel();
  await assert.rejects(q.result, /cancelled/);
});
