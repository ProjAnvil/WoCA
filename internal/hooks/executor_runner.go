package hooks

import (
	"context"

	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/runexec"
)

// ExecutorCommandRunner adapts an executor.Executor into a CommandRunner:
// the hook command runs as `sh -c` with the event payload on stdin and cwd
// as the working directory. Executor errors (startup failure or context
// cancellation) are returned unchanged; a non-zero exit code is reported via
// CommandResult.ExitCode, matching DefaultCommandRunner semantics.
//
// The executor resolves per invocation (design §10.2 M-7): a run executor
// injected into ctx (runexec.WithRunExecutor — a per-run sandbox) wins;
// otherwise the construction-time x applies, and a nil x falls back to the
// local machine.
func ExecutorCommandRunner(x executor.Executor) CommandRunner {
	return func(ctx context.Context, command, stdin, cwd string) (CommandResult, error) {
		e := x
		if rx, ok := runexec.RunExecutorFrom(ctx); ok && rx != nil {
			e = rx
		}
		if e == nil {
			e = executor.NewLocal()
		}
		res, err := e.Exec(ctx, executor.ExecRequest{
			Name:  "sh",
			Args:  []string{"-c", command},
			Dir:   cwd,
			Stdin: []byte(stdin),
		})
		out := CommandResult{
			Stdout:   string(res.Stdout),
			Stderr:   string(res.Stderr),
			ExitCode: res.ExitCode,
		}
		if err != nil {
			return out, err
		}
		return out, nil
	}
}
