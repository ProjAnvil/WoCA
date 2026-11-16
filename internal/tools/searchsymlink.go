package tools

// Symlink scope filtering (deepagents v0.7.38 parity): search results never
// leak content a symlink resolves to outside the approved search scope.
// The scope of a Glob/Grep/ListDir call is its base path; an entry behind a
// symlink whose resolution leaves that base is excluded from the results —
// the entry NAME is not hidden (0.7.35 parity), only its escaping content.

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/projanvil/woca/internal/platform/executor"
)

// readlinker is the optional executor capability that resolves symlinks.
// Local provides it; sandbox executors do not — inside a container the
// sandbox itself is the boundary, so there is nothing to filter.
type readlinker interface {
	Readlink(ctx context.Context, path string) (string, error)
}

// symlinkChainLimit caps symlink-chain resolution (a cycle must terminate,
// not hang the walk).
const symlinkChainLimit = 8

// entryEscapesBase reports whether path — an entry under base — sits behind
// a symlink whose resolution leaves base. Executors without the Readlink
// capability never filter (they cannot resolve, and their own boundary
// governs). A path not under base is not ours to judge (false).
func entryEscapesBase(ctx context.Context, x executor.Executor, base, path string) bool {
	rl, ok := x.(readlinker)
	if !ok {
		return false
	}
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	cur := filepath.Clean(base)
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		cur = filepath.Join(cur, seg)
		target, err := rl.Readlink(ctx, cur)
		if err != nil {
			continue // a regular entry, not a symlink
		}
		resolved := resolveLinkTarget(cur, target)
		for range symlinkChainLimit {
			next, err := rl.Readlink(ctx, resolved)
			if err != nil {
				break
			}
			resolved = resolveLinkTarget(resolved, next)
		}
		if escapesBase(base, resolved) {
			return true
		}
		// A link resolving INSIDE base keeps the walk on the resolved path:
		// the entries under the link name live at the resolution.
		cur = resolved
	}
	return false
}

// resolveLinkTarget joins a link target with the link's directory when the
// target is relative; absolute targets pass through.
func resolveLinkTarget(linkPath, target string) string {
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Join(filepath.Dir(linkPath), target)
}

// escapesBase reports whether resolved path leaves base.
func escapesBase(base, resolved string) bool {
	r, err := filepath.Rel(filepath.Clean(base), resolved)
	if err != nil {
		return true // mixed forms: treat as escaped
	}
	return r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r)
}
