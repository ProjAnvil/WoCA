import { spawn, type ChildProcess } from "node:child_process";
import { createInterface } from "node:readline";

export interface WocaUsage {
  requests: number; input_tokens: number; output_tokens: number;
  total_tokens: number; cache_read_tokens?: number; cache_write_tokens?: number;
}
export interface WocaToolCall { id: string; name: string; args: Record<string, unknown> }
export interface WocaMessage {
  role: string; content?: string; tool_call_id?: string; tool_calls?: WocaToolCall[];
}
export type WocaEvent =
  | { type: "assistant"; message: WocaMessage }
  | { type: "user"; message: WocaMessage }
  | WocaResult;
export interface WocaResult {
  type: "result"; subtype: "success" | "error"; result?: string;
  usage?: WocaUsage; total_cost_usd?: number; num_turns?: number;
  is_error?: boolean; error?: string;
}

export interface WocaOptions {
  model?: string; provider?: string; apiKey?: string; baseUrl?: string;
  workdir?: string; systemPrompt?: string;
  permissionMode?: "default" | "acceptEdits" | "bypassPermissions" | "readOnly" | "plan";
  maxTurns?: number; maxTokens?: number; thinkingBudget?: number; temperature?: number;
  mcpServers?: string[]; allowCommands?: string[]; denyCommands?: string[];
  db?: string; fallbackModel?: string;
  /** Explicit woca binary path; falls back to $WOCA_PATH then PATH. */
  pathToWoca?: string;
  signal?: AbortSignal;
}

function buildArgs(prompt: string, o: WocaOptions): string[] {
  const args = ["-p", prompt, "--output-format", "stream-json"];
  const push = (flag: string, v: string | number | undefined) => {
    if (v !== undefined && v !== "") args.push(flag, String(v));
  };
  push("--model", o.model); push("--provider", o.provider);
  push("--api-key", o.apiKey); push("--base-url", o.baseUrl);
  push("--workdir", o.workdir); push("--system-prompt", o.systemPrompt);
  push("--permission-mode", o.permissionMode);
  push("--max-turns", o.maxTurns); push("--max-tokens", o.maxTokens);
  push("--thinking-budget", o.thinkingBudget); push("--temperature", o.temperature);
  push("--db", o.db); push("--fallback-model", o.fallbackModel);
  for (const v of o.mcpServers ?? []) args.push("--mcp", v);
  for (const v of o.allowCommands ?? []) args.push("--allow-command", v);
  for (const v of o.denyCommands ?? []) args.push("--deny-command", v);
  return args;
}

function resolveBinary(o: WocaOptions): string {
  return o.pathToWoca ?? process.env.WOCA_PATH ?? "woca";
}

export interface WocaQuery extends AsyncIterable<WocaEvent> {
  cancel(): void;
  result: Promise<WocaResult>;
}

export function query(args: { prompt: string; options?: WocaOptions }): WocaQuery {
  const o = args.options ?? {};
  let cancelled = false;
  let resultEv: WocaResult | undefined;
  let lines: ReturnType<typeof createInterface> | undefined;

  let resolveResult!: (r: WocaResult) => void;
  let rejectResult!: (e: Error) => void;
  const result = new Promise<WocaResult>((resolve, reject) => {
    resolveResult = resolve;
    rejectResult = reject;
  });
  // Prevent unhandled rejection when the caller only iterates.
  result.catch(() => {});

  // Spawn on the next tick so callers can still mutate process.env
  // synchronously after query() before the child environment is captured.
  const ready = new Promise<ChildProcess>((resolve, reject) => {
    setImmediate(() => {
      let child: ChildProcess;
      try {
        child = spawn(resolveBinary(o), buildArgs(args.prompt, o), {
          stdio: ["ignore", "pipe", "inherit"],
          env: { ...process.env },
          ...(o.workdir ? { cwd: o.workdir } : {}),
        });
      } catch (err) {
        reject(err as Error);
        rejectResult(err as Error);
        return;
      }
      lines = createInterface({ input: child.stdout! });
      child.on("error", rejectResult);
      child.on("close", (code) => {
        if (cancelled) return rejectResult(new Error("woca query cancelled"));
        if (resultEv) return resolveResult(resultEv);
        rejectResult(new Error(`woca exited with exit code ${code} and no result event`));
      });
      if (cancelled) child.kill("SIGTERM");
      resolve(child);
    });
  });
  // Prevent unhandled rejection when the caller only uses cancel()/result.
  ready.catch(() => {});

  const cancel = () => {
    cancelled = true;
    ready.then((child) => child.kill("SIGTERM"), () => {});
  };
  o.signal?.addEventListener("abort", cancel, { once: true });

  async function* iterate(): AsyncGenerator<WocaEvent> {
    await ready;
    for await (const line of lines!) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      const ev = JSON.parse(trimmed) as WocaEvent;
      if (ev.type === "result") resultEv = ev;
      yield ev;
    }
    await result;
  }

  return { [Symbol.asyncIterator]: () => iterate(), cancel, result };
}
