"""Harbor agent adapter that runs woca headless inside a Harbor task container.

install() uploads a statically linked linux/amd64 woca binary into the task
container (built via ``make eval-binary``); run() drives
``woca -p <instruction> --output-format stream-json`` in the task working
directory with permissions bypassed, mirroring how Harbor drives Claude Code
(``claude --print``) and Codex (``codex exec``).

Quick start (from the woca repo root):

    make eval-binary
    export DEEPSEEK_API_KEY=...            # or OPENAI_API_KEY / ANTHROPIC_API_KEY
    PYTHONPATH=. harbor run --path eval/tasks/hello-woca \
        --agent eval.woca_agent:WocaAgent \
        --model deepseek/deepseek-v4-pro

Model spec mapping (harbor provider -> woca --provider): anthropic/openai/
deepseek/glm pass through natively; every other openai-compatible provider
(groq, mistral, together, ...) routes through woca's openai provider with the
resolved base URL.
"""

import json
import re
import shlex
import uuid
from pathlib import Path
from typing import Any, override

from harbor.agents.installed.base import (
    BaseInstalledAgent,
    CliFlag,
    with_prompt_template,
)
from harbor.agents.model_connection import ModelConnectionSpec
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

_BINARY_TARGET = "/usr/local/bin/woca"
_DEFAULT_BINARY = "build/woca-linux-amd64"

# harbor provider name -> woca --provider value. Providers absent from this
# map are openai-compatible and fall through to woca's openai provider with
# an explicit --base-url.
_WOCA_PROVIDERS = {
    "anthropic": "anthropic",
    "openai": "openai",
    "deepseek": "deepseek",
    "glm": "glm",
}


class WocaAgent(BaseInstalledAgent):
    """Drives the woca harness CLI as a Harbor installed agent."""

    # No default_provider: the provider must come from the model spec prefix
    # (deepseek/..., anthropic/..., groq/...) so provider-specific key envs
    # and official base URLs resolve from Harbor's PROVIDERS table. A prefix
    # Harbor cannot classify falls back to woca's openai provider below.
    MODEL_CONNECTION = ModelConnectionSpec(
        api_key_envs=("OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ZAI_API_KEY"),
        base_url_envs=("OPENAI_BASE_URL",),
    )

    CLI_FLAGS = [
        CliFlag(
            "max_turns",
            cli="--max-turns",
            type="int",
            env_fallback="WOCA_MAX_TURNS",
        ),
        CliFlag(
            "thinking_budget",
            cli="--thinking-budget",
            type="int",
            env_fallback="WOCA_THINKING_BUDGET",
        ),
        CliFlag(
            "temperature",
            cli="--temperature",
            type="str",
            env_fallback="WOCA_TEMPERATURE",
        ),
        CliFlag("fallback_model", cli="--fallback-model", type="str"),
        CliFlag("extra_prompt", cli="--system-prompt", type="str"),
        CliFlag(
            "permission_mode",
            cli="--permission-mode",
            type="enum",
            choices=[
                "default",
                "acceptEdits",
                "bypassPermissions",
                "readOnly",
                "plan",
            ],
            default="bypassPermissions",
            format="--permission-mode={value}",
        ),
    ]

    def __init__(
        self, logs_dir: Path, binary_path: str | None = None, *args: Any, **kwargs: Any
    ):
        self._binary_path = binary_path
        super().__init__(logs_dir, *args, **kwargs)

    @staticmethod
    @override
    def name() -> str:
        return "woca"

    def _resolve_binary(self) -> Path:
        raw = self._binary_path or self._get_env("WOCA_BINARY_PATH") or _DEFAULT_BINARY
        path = Path(raw).expanduser()
        if not path.is_absolute():
            path = Path.cwd() / path
        if not path.is_file():
            raise FileNotFoundError(
                f"woca linux/amd64 binary not found at {path}. Build it with "
                "`make eval-binary`, or pass --ak binary_path=... / set "
                "WOCA_BINARY_PATH."
            )
        return path

    @override
    def get_version_command(self) -> str | None:
        return f"{_BINARY_TARGET} version"

    @override
    def parse_version(self, stdout: str) -> str:
        match = re.search(r"(\d+\.\d+\.\d+)", stdout)
        return match.group(1) if match else stdout.strip()

    @override
    async def install(self, environment: BaseEnvironment) -> None:
        binary = self._resolve_binary()
        # upload_file lands as root; make it world-executable for any task user.
        await environment.upload_file(binary, _BINARY_TARGET)
        await self.exec_as_root(
            environment,
            command=(
                f"chmod 755 {shlex.quote(_BINARY_TARGET)} && {_BINARY_TARGET} version"
            ),
        )

    @override
    @with_prompt_template
    async def run(
        self, instruction: str, environment: BaseEnvironment, context: AgentContext
    ) -> None:
        access = self.model_connection
        if not self._parsed_model_name:
            raise ValueError(
                "WocaAgent requires --model (e.g. deepseek/deepseek-v4-pro)"
            )

        woca_provider = _WOCA_PROVIDERS.get(access.provider or "", "openai")

        # The instruction and API key travel through environment variables into
        # shell variables so neither appears in the logged command line; the
        # key var is unset before woca starts so it never reaches the agent
        # process environment (it only sees the CLI flag).
        run_env: dict[str, str] = {}
        key_var = f"harbor_woca_key_{uuid.uuid4().hex}"
        if access.api_key:
            run_env[key_var.upper()] = access.api_key
        base_url_flag = ""
        if access.configured_base_url:
            base_url_flag = f" --base-url {shlex.quote(access.configured_base_url)}"

        cli_flags = self.build_cli_flags()
        extra_flags = f" {cli_flags}" if cli_flags else ""

        instruction_var = f"harbor_woca_instruction_{uuid.uuid4().hex}"
        run_env[instruction_var.upper()] = instruction

        log_path = (self.environment_logs_dir / "woca.txt").as_posix()
        api_key_flag = f' --api-key "${key_var}"' if access.api_key else ""
        unset_vars = " ".join(
            var for var in (key_var.upper(), instruction_var.upper()) if var in run_env
        )

        await self.exec_as_agent(
            environment,
            command=(
                f'{instruction_var}="${instruction_var.upper()}"; '
                f"{key_var}=\"${key_var.upper()}\"; "
                f"unset {unset_vars}; "
                f'woca -p "${instruction_var}"'
                f" --provider {shlex.quote(woca_provider)}"
                f" --model {shlex.quote(self._parsed_model_name)}"
                f"{base_url_flag}"
                f"{api_key_flag}"
                " --output-format stream-json"
                " --db none"
                f"{extra_flags}"
                f" 2>&1 | tee {shlex.quote(log_path)}"
            ),
            env=run_env,
        )

    @override
    def populate_context_post_run(self, context: AgentContext) -> None:
        """Fill usage metrics from the final stream-json result event.

        woca's sdkout emits a Claude-Code-compatible result envelope
        ({"type":"result", "usage": ..., "total_cost_usd": ...}) as the last
        stream line, teed to <logs_dir>/woca.txt on the host.
        """
        try:
            content = (self.logs_dir / "woca.txt").read_text(encoding="utf-8")
        except OSError:
            return

        result_event: dict[str, Any] | None = None
        for line in content.splitlines():
            line = line.strip()
            if not line.startswith("{"):
                continue
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            if event.get("type") == "result":
                result_event = event
        if not result_event:
            return

        def usage_int(*keys: str) -> int:
            usage = result_event.get("usage") or {}
            for key in keys:
                value = usage.get(key)
                if isinstance(value, (int, float)):
                    return int(value)
            return 0

        context.n_input_tokens = usage_int("input_tokens", "prompt_tokens")
        context.n_output_tokens = usage_int("output_tokens", "completion_tokens")
        cost = result_event.get("total_cost_usd")
        if isinstance(cost, (int, float)):
            context.cost_usd = float(cost)
