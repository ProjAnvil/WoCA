import asyncio
import os
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "src"))

from woca_agent_sdk import WocaOptions, query

FAKE = str(Path(__file__).resolve().parent / "fake_woca.sh")


def run(coro):
    return asyncio.run(coro)


def test_streams_events_and_result():
    async def main():
        events = [ev async for ev in query("hi", WocaOptions(path_to_woca=FAKE))]
        return events

    events = run(main())
    assert [e["type"] for e in events] == ["assistant", "assistant", "user", "result"]
    result = events[-1]
    assert result["subtype"] == "success"
    assert result["usage"]["input_tokens"] == 10


def test_maps_options_to_flags():
    async def main():
        async for ev in query(
            "hi",
            WocaOptions(
                path_to_woca=FAKE, model="m", max_turns=3,
                permission_mode="bypassPermissions", deny_commands=["rm -rf"], db="none",
            ),
        ):
            return ev  # first event echoes the CLI args

    echoed = run(main())["message"]["content"]
    for flag in ["--model m", "--max-turns 3", "--permission-mode bypassPermissions",
                 "--deny-command rm -rf", "--db none", "--output-format stream-json"]:
        assert flag in echoed, f"missing {flag} in {echoed}"


def test_nonzero_exit_raises():
    os.environ["FAKE_WOCA_FAIL"] = "1"
    try:
        async def main():
            async for _ in query("hi", WocaOptions(path_to_woca=FAKE)):
                pass

        with pytest.raises(RuntimeError, match="exit code 1"):
            run(main())
    finally:
        del os.environ["FAKE_WOCA_FAIL"]


def test_early_exit_terminates_process(monkeypatch):
    os.environ["FAKE_WOCA_HANG"] = "1"
    captured = {}
    real_spawn = asyncio.create_subprocess_exec

    async def spy(*args, **kwargs):
        proc = await real_spawn(*args, **kwargs)
        captured["proc"] = proc
        return proc

    monkeypatch.setattr(asyncio, "create_subprocess_exec", spy)
    try:
        async def main():
            agen = query("hi", WocaOptions(path_to_woca=FAKE))
            first = await agen.__anext__()
            assert first["type"] == "assistant"
            await agen.aclose()
            return captured["proc"].returncode

        # after aclose() the finally block must have terminated the child
        assert run(main()) is not None
    finally:
        del os.environ["FAKE_WOCA_HANG"]
