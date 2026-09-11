"""Drive the woca agent harness headlessly via `woca -p --output-format stream-json`."""

from __future__ import annotations

import asyncio
import json
import os
import shutil
from dataclasses import dataclass, field
from typing import AsyncIterator, Optional


@dataclass
class WocaOptions:
    model: Optional[str] = None
    provider: Optional[str] = None
    api_key: Optional[str] = None
    base_url: Optional[str] = None
    workdir: Optional[str] = None
    system_prompt: Optional[str] = None
    permission_mode: Optional[str] = None  # default|acceptEdits|bypassPermissions|readOnly|plan
    max_turns: Optional[int] = None
    max_tokens: Optional[int] = None
    thinking_budget: Optional[int] = None
    temperature: Optional[float] = None
    mcp_servers: list[str] = field(default_factory=list)
    allow_commands: list[str] = field(default_factory=list)
    deny_commands: list[str] = field(default_factory=list)
    db: Optional[str] = None  # "none" disables persistence
    fallback_model: Optional[str] = None
    path_to_woca: Optional[str] = None  # falls back to $WOCA_PATH then PATH


def _build_args(prompt: str, o: WocaOptions) -> list[str]:
    args = ["-p", prompt, "--output-format", "stream-json"]

    def push(flag: str, value) -> None:
        if value is not None and value != "":
            args.extend([flag, str(value)])

    push("--model", o.model)
    push("--provider", o.provider)
    push("--api-key", o.api_key)
    push("--base-url", o.base_url)
    push("--workdir", o.workdir)
    push("--system-prompt", o.system_prompt)
    push("--permission-mode", o.permission_mode)
    push("--max-turns", o.max_turns)
    push("--max-tokens", o.max_tokens)
    push("--thinking-budget", o.thinking_budget)
    push("--temperature", o.temperature)
    push("--db", o.db)
    push("--fallback-model", o.fallback_model)
    for v in o.mcp_servers:
        args += ["--mcp", v]
    for v in o.allow_commands:
        args += ["--allow-command", v]
    for v in o.deny_commands:
        args += ["--deny-command", v]
    return args


def _resolve_binary(o: WocaOptions) -> str:
    if o.path_to_woca:
        return o.path_to_woca
    if env := os.environ.get("WOCA_PATH"):
        return env
    if found := shutil.which("woca"):
        return found
    raise FileNotFoundError("woca binary not found; set WocaOptions.path_to_woca or $WOCA_PATH")


async def query(prompt: str, options: Optional[WocaOptions] = None) -> AsyncIterator[dict]:
    """Run one headless woca session, yielding stream-json events as dicts.

    The last event is always the result envelope (type == "result").
    Raises RuntimeError when woca exits non-zero without a result event.
    Closing the generator early (aclose() / GC) terminates the child process.
    """
    o = options or WocaOptions()
    proc = await asyncio.create_subprocess_exec(
        _resolve_binary(o), *_build_args(prompt, o),
        stdout=asyncio.subprocess.PIPE,
        stderr=None,  # inherit: human diagnostics stay visible
        cwd=o.workdir,
        env={**os.environ},
        limit=16 * 1024 * 1024,  # default 64KiB line cap chokes on large tool output
    )
    assert proc.stdout is not None
    saw_result = False
    try:
        async for raw in proc.stdout:
            line = raw.decode().strip()
            if not line:
                continue
            ev = json.loads(line)
            if ev.get("type") == "result":
                saw_result = True
            yield ev
        code = await proc.wait()
        if not saw_result and code != 0:
            raise RuntimeError(f"woca exited with exit code {code} and no result event")
    finally:
        if proc.returncode is None:
            try:
                proc.terminate()
            except ProcessLookupError:
                pass
            await proc.wait()
