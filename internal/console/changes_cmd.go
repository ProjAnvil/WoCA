package console

import (
	"context"
	"fmt"
	"strings"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/diffutil"
)

// /changes: the pending-workspace preview behind /undo — every undoable
// Write/Edit/NotebookEdit mutation with per-file +/− stats, so /undo is
// look-before-you-leap instead of blind stepping. The data comes from the
// agent's file-history checkpoint stack (the same source the web UI's
// Changes tab reads through GET /api/workspace/changes).

// changesCmd implements /changes: no args lists pending files with stats;
// `/changes full` also prints each file's unified diff.
func (c *Console) changesCmd(args []string) (Result, error) {
	ctx := agent.WithRunWorkspace(context.Background(), c.deps.Cfg.WorkDir)
	changes := c.deps.Agent.PendingFiles(ctx)
	undoN, redoN := c.deps.Agent.FileHistoryCounts(ctx)
	if len(changes) == 0 {
		return Result{Output: fmt.Sprintf("no pending changes (%d undoable, %d redoable steps)\n", undoN, redoN)}, nil
	}
	full := len(args) > 0 && args[0] == "full"
	var b strings.Builder
	fmt.Fprintf(&b, "pending changes (%d files · %d undoable, %d redoable steps):\n", len(changes), undoN, redoN)
	for _, ch := range changes {
		rel := ch.Path
		if rest, ok := strings.CutPrefix(ch.Path, c.deps.Cfg.WorkDir+"/"); ok {
			rel = rest
		}
		switch {
		case ch.Before == nil:
			fmt.Fprintf(&b, "  + new  %s\n", rel)
		case ch.After == nil:
			fmt.Fprintf(&b, "  - del  %s\n", rel)
		default:
			ops := diffutil.Diff(diffutil.SplitLines(string(ch.Before)), diffutil.SplitLines(string(ch.After)))
			added, removed := diffutil.Counts(ops)
			fmt.Fprintf(&b, "  ~ mod  %s  (+%d −%d)\n", rel, added, removed)
		}
		if full {
			b.WriteString(diffutil.Unified(rel, ch.Before, ch.After, 3))
			b.WriteString("\n")
		}
	}
	b.WriteString("revert with /undo (one step at a time, newest first)\n")
	return Result{Output: b.String()}, nil
}
