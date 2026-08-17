package tools

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	coretools "github.com/projanvil/langchain-golang/core/tools"
)

const bashDescription = `Run a shell command in the working directory. Returns combined stdout/stderr and the exit code. Use for inspecting the repo, running builds/tests, and any command the other tools cannot do.

Long-running commands (builds, dev servers, watchers) should set run_in_background=true: the command starts detached, the tool returns a shell ID immediately, and a completion notification with the output tail arrives when it finishes. Poll interim output with the BashOutput tool and terminate early with KillShell. Foreground commands are killed after timeout_ms (default 120s).`

type bashArgs struct {
	Command         string `json:"command"`
	TimeoutMs       int    `json:"timeout_ms,omitempty"`
	Workdir         string `json:"workdir,omitempty"`
	RunInBackground bool   `json:"run_in_background,omitempty"`
}

func bashTool(d *Dir, bg *BackgroundShells) coretools.Tool {
	t, err := coretools.FromFunc("Bash", bashDescription,
		func(ctx context.Context, a bashArgs) (coretools.Result, error) {
			baseWorkdir := d.For(ctx)
			wd := a.Workdir
			if wd == "" {
				wd = baseWorkdir
			}
			if !filepath.IsAbs(wd) {
				wd = filepath.Join(baseWorkdir, wd)
			}

			if a.RunInBackground {
				id, err := bg.Start(a.Command, a.Workdir, baseWorkdir)
				if err != nil {
					return coretools.Result{Content: fmt.Sprintf("Error: start background command: %v", err)}, nil
				}
				return coretools.Result{Content: fmt.Sprintf("[Background] started shell %s: %s\nUse BashOutput to poll output, KillShell to stop it. A completion notification will arrive when it finishes.", id, a.Command)}, nil
			}

			timeout := time.Duration(a.TimeoutMs) * time.Millisecond
			if timeout <= 0 {
				timeout = 120 * time.Second
			}
			runCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			cmd := exec.CommandContext(runCtx, "bash", "-c", a.Command)
			cmd.Dir = wd
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			runErr := cmd.Run()

			out := stdout.String()
			if stderr.Len() > 0 {
				if out != "" {
					out += "\n"
				}
				out += stderr.String()
			}

			if runErr != nil {
				if runCtx.Err() == context.DeadlineExceeded {
					return coretools.Result{Content: fmt.Sprintf("Command timed out after %s.\n%s", timeout, out)}, nil
				}
				exit := -1
				if ee, ok := runErr.(*exec.ExitError); ok {
					exit = ee.ExitCode()
				}
				return coretools.Result{Content: fmt.Sprintf("Exit code %d.\n%s", exit, out)}, nil
			}
			if out == "" {
				out = "(no output)"
			}
			return coretools.Result{Content: out}, nil
		})
	if err != nil {
		panic(err)
	}
	return t
}
