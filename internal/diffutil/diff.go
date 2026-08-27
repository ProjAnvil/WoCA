// Package diffutil computes small unified diffs for the web UI's Changes
// review tab. It implements the classic Myers O(ND) shortest-edit-script
// algorithm over lines, keeping one diagonal-index map per d for
// backtracking; pathologically different inputs fall back to a single
// whole-file replace hunk so cost stays bounded.
package diffutil

import (
	"fmt"
	"strings"
)

// Op is one line of the edit script: '=' context, '-' removed, '+' added.
type Op struct {
	Kind byte
	Line string
}

// maxDepth bounds the Myers search: beyond it the diff degrades to a
// replace-all (still correct, just coarser).
const maxDepth = 2000

// Diff returns the edit script transforming a into b.
func Diff(a, b []string) []Op {
	n, m := len(a), len(b)
	maxD := n + m
	if maxD > maxDepth {
		return replaceAll(a, b)
	}

	// trace[d] is the diagonal map as of d rounds completed minus one —
	// the state the d-th round's moves read from (v[k] = furthest x on
	// diagonal k, where x-y = k).
	v := map[int]int{1: 0}
	var trace []map[int]int
	found := false
	for d := 0; d <= maxD && !found; d++ {
		trace = append(trace, copyMap(v))
		next := map[int]int{}
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[k-1] < v[k+1]) {
				x = v[k+1]
			} else {
				x = v[k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			next[k] = x
			if x >= n && y >= m {
				found = true
				break
			}
		}
		v = next
	}
	if !found {
		return replaceAll(a, b)
	}
	return canonicalOrder(backtrack(trace, a, b))
}

// backtrack walks the traces from (len(a), len(b)) back to the origin,
// yielding the edit script in forward order.
func backtrack(trace []map[int]int, a, b []string) []Op {
	type move struct {
		kind byte
		line string
	}
	var rev []move
	x, y := len(a), len(b)
	for d := len(trace) - 1; d >= 0; d-- {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[k-1] < v[k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			rev = append(rev, move{'=', a[x-1]})
			x--
			y--
		}
		if d > 0 {
			if x == prevX {
				rev = append(rev, move{'+', b[y-1]})
				y--
			} else {
				rev = append(rev, move{'-', a[x-1]})
				x--
			}
		}
		x, y = prevX, prevY
	}
	// The backtrack walks end→start; the script reads start→end.
	ops := make([]Op, len(rev))
	for i, mv := range rev {
		ops[len(rev)-1-i] = Op{Kind: mv.kind, Line: mv.line}
	}
	return ops
}

func replaceAll(a, b []string) []Op {
	ops := make([]Op, 0, len(a)+len(b))
	for _, l := range a {
		ops = append(ops, Op{'-', l})
	}
	for _, l := range b {
		ops = append(ops, Op{'+', l})
	}
	return ops
}

func copyMap(m map[int]int) map[int]int {
	out := make(map[int]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// SplitLines splits s into newline-free lines; empty input yields nil.
func SplitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// Unified renders a unified diff between before and after (nil = the file
// did not exist on that side). It returns the diff text (empty when the
// sides are identical) plus the added/removed line counts.
func Unified(path string, before, after []byte, context int) string {
	a := SplitLines(string(before))
	b := SplitLines(string(after))
	ops := Diff(a, b)
	added, removed := 0, 0
	for _, op := range ops {
		switch op.Kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	if added == 0 && removed == 0 {
		return ""
	}

	var out []string
	out = append(out, "--- a/"+path, "+++ b/"+path)
	i := 0
	for i < len(ops) {
		if ops[i].Kind == '=' {
			i++
			continue
		}
		// Hunk extent: changes merge while the gap between them stays
		// within 2*context equal lines; then pad with context on both sides.
		lastChange := i
		for j := i; j < len(ops); j++ {
			if ops[j].Kind != '=' {
				lastChange = j
			} else if j-lastChange > context*2 {
				break
			}
		}
		lead := i
		for k := 0; k < context && lead > 0 && ops[lead-1].Kind == '='; k++ {
			lead--
		}
		trail := lastChange
		for k := 0; k < context && trail+1 < len(ops) && ops[trail+1].Kind == '='; k++ {
			trail++
		}
		aPos, bPos := countUpTo(ops, lead)
		hunkA, hunkB := 0, 0
		var body []string
		for j := lead; j <= trail; j++ {
			switch ops[j].Kind {
			case '=':
				body = append(body, " "+ops[j].Line)
				hunkA++
				hunkB++
			case '-':
				body = append(body, "-"+ops[j].Line)
				hunkA++
			case '+':
				body = append(body, "+"+ops[j].Line)
				hunkB++
			}
		}
		out = append(out, fmt.Sprintf("@@ -%d,%d +%d,%d @@", aPos+1, hunkA, bPos+1, hunkB))
		out = append(out, body...)
		i = trail + 1
	}
	return strings.Join(out, "\n") + "\n"
}

// canonicalOrder stable-partitions each maximal change run so deletions
// precede additions (the conventional unified-diff shape: -old then +new).
func canonicalOrder(ops []Op) []Op {
	out := make([]Op, 0, len(ops))
	i := 0
	for i < len(ops) {
		if ops[i].Kind == '=' {
			out = append(out, ops[i])
			i++
			continue
		}
		j := i
		for j < len(ops) && ops[j].Kind != '=' {
			j++
		}
		for k := i; k < j; k++ {
			if ops[k].Kind == '-' {
				out = append(out, ops[k])
			}
		}
		for k := i; k < j; k++ {
			if ops[k].Kind == '+' {
				out = append(out, ops[k])
			}
		}
		i = j
	}
	return out
}

// Counts returns the added/removed line counts of an edit script.
func Counts(ops []Op) (added, removed int) {
	for _, op := range ops {
		switch op.Kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	return added, removed
}

func countUpTo(ops []Op, idx int) (a, b int) {
	for i := 0; i < idx; i++ {
		switch ops[i].Kind {
		case '=':
			a++
			b++
		case '-':
			a++
		case '+':
			b++
		}
	}
	return a, b
}
