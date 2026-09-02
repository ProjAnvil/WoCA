// Package merge implements layered configuration merging for skills,
// agents and MCP servers: entries from higher-priority levels (flag >
// workspace > user) win, same-name conflicts are recorded for observability,
// and the merged output order is deterministic. The package stays free of
// woca-internal imports so it stays trivially testable.
package merge

import "sort"

// Level is a configuration source layer. Higher values win over lower ones.
type Level int

const (
	LevelBuiltin   Level = -2   // shipped in the binary (vendored method skills)
	LevelManaged   Level = -1   // operator-delivered via the config plane
	LevelUser      Level = iota // ~/.woca (user level)
	LevelWorkspace              // workDir and its ancestors up to the git root
	LevelFlag                   // session injection (--mcp etc.), highest
)

// String returns the log-friendly layer name.
func (l Level) String() string {
	switch l {
	case LevelFlag:
		return "flag"
	case LevelWorkspace:
		return "workspace"
	case LevelManaged:
		return "managed"
	case LevelBuiltin:
		return "builtin"
	default:
		return "user"
	}
}

// Entry is one named candidate from a source layer.
type Entry[T any] struct {
	Name   string
	Value  T
	Level  Level
	Order  int    // tiebreaker within a Level; smaller wins
	Source string // file path or "flag", for conflict logs
}

// Conflict records one same-name resolution. Winner and Loser are Sources.
type Conflict struct {
	Name   string
	Winner string
	Loser  string
}

// Entries merges es by Name: higher Level wins, then smaller Order, then
// lexicographic Source (total determinism). The result is sorted by
// (Level desc, Order asc, Name asc); every same-name collision appends one
// Conflict, except fully duplicate entries (same Source), which are skipped.
// Entries with an empty Name are dropped.
func Entries[T any](es []Entry[T]) ([]Entry[T], []Conflict) {
	byName := make(map[string]Entry[T], len(es))
	var conflicts []Conflict
	for _, e := range es {
		if e.Name == "" {
			continue
		}
		prev, ok := byName[e.Name]
		if !ok {
			byName[e.Name] = e
			continue
		}
		winner, loser := pick(prev, e)
		byName[e.Name] = winner
		if winner.Source != loser.Source {
			conflicts = append(conflicts, Conflict{Name: e.Name, Winner: winner.Source, Loser: loser.Source})
		}
	}
	out := make([]Entry[T], 0, len(byName))
	for _, e := range byName {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Level != out[j].Level {
			return out[i].Level > out[j].Level
		}
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Name < out[j].Name
	})
	return out, conflicts
}

// pick returns the winning and losing entry (winner first).
func pick[T any](a, b Entry[T]) (Entry[T], Entry[T]) {
	if a.Level != b.Level {
		if a.Level > b.Level {
			return a, b
		}
		return b, a
	}
	if a.Order != b.Order {
		if a.Order < b.Order {
			return a, b
		}
		return b, a
	}
	if a.Source <= b.Source {
		return a, b
	}
	return b, a
}
