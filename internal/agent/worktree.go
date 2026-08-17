package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	coretools "github.com/projanvil/langchain-golang/core/tools"
)

// Worktree isolation: EnterWorktree creates a git worktree under
// <base>/.woca-worktrees/<name>
// and switches the tool registry's working directory into it, so every
// file/shell tool operates on the isolated copy without disturbing the main
// checkout. ExitWorktree returns to the original directory and can remove
// the worktree again. The agent's prompt-time cwd (new subagents, prompts)
// still names the base directory.

const worktreeDescription = `Enter an isolated git worktree: creates a new worktree (and branch woca/<name>) under .woca-worktrees/ and switches all file/shell tools to operate inside it. Use for parallel isolated changes. Exit with ExitWorktree.`

const exitWorktreeDescription = `Leave the current worktree, returning all tools to the original working directory. Set remove=true to also delete the worktree directory and its branch.`

type enterWorktreeArgs struct {
	Name string `json:"name"`
	Ref  string `json:"ref,omitempty"` // branch or commit to base the worktree on (default HEAD)
}

type exitWorktreeArgs struct {
	Remove bool `json:"remove,omitempty"`
}

func (a *Agent) worktreeTools() []coretools.Tool {
	enter, err := coretools.FromFunc("EnterWorktree", worktreeDescription,
		func(ctx context.Context, args enterWorktreeArgs) (coretools.Result, error) {
			name := strings.Map(sanitizeWorktreeRune, args.Name)
			if name == "" {
				return coretools.Result{Content: "Error: a worktree name is required"}, nil
			}
			d := a.registry.Dir()
			base := d.BaseFor(ctx)
			if !isGitRepo(base) {
				return coretools.Result{Content: "Error: not a git repository (" + base + ")"}, nil
			}
			if cur := d.For(ctx); cur != base {
				return coretools.Result{Content: "Error: already in a worktree (" + cur + "); ExitWorktree first"}, nil
			}
			wtDir := filepath.Join(base, ".woca-worktrees", name)
			var gitArgs []string
			if args.Ref == "" {
				gitArgs = []string{"worktree", "add", "-b", "woca/" + name, wtDir}
			} else {
				gitArgs = []string{"worktree", "add", wtDir, args.Ref}
			}
			cmd := exec.Command("git", gitArgs...)
			cmd.Dir = base
			out, err := cmd.CombinedOutput()
			if err != nil {
				return coretools.Result{Content: fmt.Sprintf("Error: git worktree add: %v\n%s", err, strings.TrimSpace(string(out)))}, nil
			}
			d.Enter(ctx, wtDir)
			return coretools.Result{Content: fmt.Sprintf("Entered worktree %s (%s). All tools now operate there.", name, wtDir)}, nil
		})
	if err != nil {
		panic(err)
	}
	exit, err := coretools.FromFunc("ExitWorktree", exitWorktreeDescription,
		func(ctx context.Context, args exitWorktreeArgs) (coretools.Result, error) {
			d := a.registry.Dir()
			base := d.BaseFor(ctx)
			cur, inWorktree := d.Exit(ctx)
			if !inWorktree {
				return coretools.Result{Content: "Not currently in a worktree."}, nil
			}
			if !args.Remove {
				return coretools.Result{Content: "Left the worktree; tools operate on " + base + " again."}, nil
			}
			cmd := exec.Command("git", "worktree", "remove", "--force", cur)
			cmd.Dir = base
			out, err := cmd.CombinedOutput()
			if err != nil {
				return coretools.Result{Content: fmt.Sprintf("Left the worktree, but removal failed: %v\n%s", err, strings.TrimSpace(string(out)))}, nil
			}
			return coretools.Result{Content: "Left and removed the worktree; tools operate on " + base + " again."}, nil
		})
	if err != nil {
		panic(err)
	}
	return []coretools.Tool{enter, exit}
}

func sanitizeWorktreeRune(r rune) rune {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		return r
	default:
		return -1
	}
}

func isGitRepo(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && (info.IsDir() || info.Mode().IsRegular())
}
