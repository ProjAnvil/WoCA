package tools

import "strings"

// CommandPolicy decides whether a shell command may run.
type CommandPolicy struct {
	// Allowlist, when non-empty, is the only set of command prefixes that may
	// run (after stripping wrappers like "sudo", "env", "cd <dir> &&").
	Allowlist []string
	// Denylist blocks commands containing any of these substrings (case-
	// insensitive).
	Denylist []string
}

// NewCommandPolicy builds a policy (nil slices become empty).
func NewCommandPolicy(allowlist, denylist []string) CommandPolicy {
	a := make([]string, len(allowlist))
	copy(a, allowlist)
	d := make([]string, len(denylist))
	copy(d, denylist)
	return CommandPolicy{Allowlist: a, Denylist: d}
}

// Allow reports whether command may run, returning (true, "") when allowed or
// (false, reason) when blocked. Denylist takes precedence over allowlist.
func (p CommandPolicy) Allow(command string) (bool, string) {
	if len(p.Denylist) > 0 {
		lower := strings.ToLower(command)
		for _, entry := range p.Denylist {
			if strings.Contains(lower, strings.ToLower(entry)) {
				return false, "blocked by denylist"
			}
		}
	}

	if len(p.Allowlist) > 0 {
		body := strings.ToLower(stripEnvAndChaining(command))
		for _, entry := range p.Allowlist {
			if strings.HasPrefix(body, strings.ToLower(strings.TrimSpace(entry))) {
				return true, ""
			}
		}
		return false, "not in allowlist"
	}

	return true, ""
}
