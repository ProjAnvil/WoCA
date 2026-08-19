package hooks

import (
	"context"

	"github.com/projanvil/woca/internal/platform/executor"
)

// ExecutorCommandRunner adapts an executor.Executor into a CommandRunner:
// the hook command runs as `sh -c` with the event payload on stdin and cwd
// as the working directory. Executor errors (startup failure or context
// cancellation) are returned unchanged; a non-zero exit code is reported via
// CommandResult.ExitCode, matching DefaultCommandRunner semantics.
func ExecutorCommandRunner(x executor.Executor) CommandRunner {
	return func(ctx context.Context, command, stdin, cwd string) (CommandResult, error) {
		res, err := x.Exec(ctx, executor.ExecRequest{
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
