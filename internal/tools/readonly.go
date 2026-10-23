package tools

import (
	"slices"
	"strings"
)

// readOnlyCommandPrefixes are command prefixes whose effect is to inspect, not
// mutate, the filesystem or process state. This is a deliberately conservative
// subset of a read-only Bash classifier.
var readOnlyCommandPrefixes = []string{
	"ls", "ll", "l.", "dir",
	"cat", "head", "tail", "more", "less", "zcat",
	"grep", "egrep", "fgrep", "rg", "ag",
	"find", "locate", "which", "whereis", "type", "command -v",
	"pwd", "echo", "printf", "date", "cal", "uname", "hostname", "whoami", "id",
	"wc", "sort", "uniq", "cut", "tr", "paste", "join", "column", "awk", "sed -n",
	"diff", "cmp", "comm", "file", "stat", "du", "df", "mount", "ps", "top",
	"history", "env", "printenv", "set", "ulimit", "umask",
	"git status", "git log", "git diff", "git show", "git branch", "git tag",
	"git remote", "git ls-files", "git grep", "git rev-parse", "git rev-list",
	"git describe", "git blame", "git shortlog", "git stash list", "git config",
	"git worktree list", "git submodule status",
	"go env", "go list", "go doc", "go version", "go vet", "go fmt",
	"cargo metadata", "cargo search", "rustc --version",
	"npm ls", "npm view", "npm outdated", "npm --version", "node --version",
	"pnpm ls", "yarn list", "yarn outdated", "bun pm ls",
	"python --version", "python3 --version", "pip list", "pip show",
	"docker ps", "docker images", "docker inspect", "docker logs", "docker stats",
	"docker top", "docker version", "docker info", "docker compose ps",
	"docker compose config",
	"kubectl get", "kubectl describe", "kubectl logs", "kubectl config view",
	"kubectl explain",
	"helm list", "helm status", "helm get", "helm history",
	"terraform plan", "terraform state list", "terraform show", "terraform output",
	"gh pr view", "gh issue list", "gh repo view", "gh status",
}

// writeIndicators, when present, mark a command as non-read-only even if its
// prefix looks read-only (e.g. "ls > file.txt", "git diff > patch").
var writeIndicators = []string{
	">", ">>", " -o ", " --output",
	"git add", "git commit", "git push", "git pull", "git checkout", "git reset",
	"git merge", "git rebase", "git clean", "git rm", "git clone", "git switch",
	"git restore", "git stash push", "git stash pop", "git stash apply",
	"git stash drop", "git stash clear", "git stash save", "git stash branch",
	"git fetch",
	"npm install", "npm run", "pnpm install", "yarn add", "cargo build", "go build",
	"go run", "go test", "go install", "docker run", "docker build",
}

// writeCommands are whole command words that mutate state. They are matched on
// word boundaries so that, e.g., "rm" does not match the end of "terraform".
var writeCommands = []string{
	"install", "uninstall", "mkdir", "touch", "rm", "rmdir", "mv", "cp",
	"chmod", "chown", "ln", "curl", "wget", "tee", "make", "dd", "mkfs",
	"format", "kill", "killall", "shutdown", "reboot",
}

// destructiveCommands are whole command words that delete data, drop state, or
// kill/restart the machine. Matched on word boundaries via containsCommand.
var destructiveCommands = []string{
	"rm", "rmdir", "mkfs", "shutdown", "reboot", "kill", "killall",
	"format", "deltree", "truncate", "rebase", "erase", "del", "dd",
}

// destructiveCommandPhrases are multi-word command fragments that are
// destructive or hard to reverse (history rewrites, force pushes, SQL drops).
// containsCommand applies word boundaries to the whole phrase, so "git clean"
// does not match "git cleanup" and "git push --force" does not match
// "git push --force-with-lease".
var destructiveCommandPhrases = []string{
	"git reset --hard",
	"git push --force-with-lease",
	"git push --force",
	"git push -f",
	"git clean",
	"git reflog delete",
	"git reflog expire",
	"git reflog drop",
	"drop table",
	"drop database",
	"rm -rf",
}

// IsReadOnlyCommand reports whether a shell command only reads state. It is a
// conservative heuristic: unknown commands are treated as non-read-only.
//
// Leading wrappers (sudo, command, env, nice, nohup, timeout), environment
// assignments, and "cd <dir> &&" / "cd <dir>;" chains are stripped, and a
// compound command ("&&", ";", "||", "|") is read-only only when every
// segment is read-only.
func IsReadOnlyCommand(command string) bool {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return false
	}

	body := stripEnvAndChaining(trimmed)
	segments := splitCompound(body)
	if len(segments) == 0 {
		return false
	}
	for _, seg := range segments {
		if !isReadOnlySegment(seg) {
			return false
		}
	}
	return true
}

// IsDestructiveCommand reports whether a shell command is destructive or hard
// to reverse (deletes data, rewrites history, force-pushes, drops, etc.).
//
// Like IsReadOnlyCommand, leading wrappers (sudo, command, env, nice, nohup,
// timeout), environment assignments, and "cd <dir> &&" / "cd <dir>;" chains are
// stripped, and a compound command ("&&", ";", "||", "|") is destructive when
// any segment is destructive.
func IsDestructiveCommand(command string) bool {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return false
	}

	body := stripEnvAndChaining(trimmed)
	segments := splitCompound(body)
	return slices.ContainsFunc(segments, isDestructiveSegment)
}

// isDestructiveSegment classifies a single segment (no compound operators) as
// destructive.
func isDestructiveSegment(seg string) bool {
	body := stripEnvAndChaining(seg)
	if body == "" {
		return false
	}
	bodyLower := strings.ToLower(body)

	for _, cmd := range destructiveCommands {
		if containsCommand(bodyLower, cmd) {
			return true
		}
	}
	for _, phrase := range destructiveCommandPhrases {
		if containsCommand(bodyLower, phrase) {
			return true
		}
	}
	return false
}

// isReadOnlySegment classifies a single segment (no compound operators) as
// read-only.
func isReadOnlySegment(seg string) bool {
	body := stripEnvAndChaining(seg)
	if body == "" {
		return false
	}
	bodyLower := strings.ToLower(body)

	for _, ind := range writeIndicators {
		if strings.Contains(bodyLower, ind) {
			return false
		}
	}
	for _, cmd := range writeCommands {
		if containsCommand(bodyLower, cmd) {
			return false
		}
	}

	// aws <service> describe/get are read-only; aws is otherwise not trusted.
	if strings.HasPrefix(bodyLower, "aws ") {
		if strings.Contains(bodyLower, " describe") || strings.Contains(bodyLower, " get") {
			return true
		}
	}

	for _, prefix := range readOnlyCommandPrefixes {
		if strings.HasPrefix(bodyLower, prefix) {
			return true
		}
	}
	return false
}

// isWordByte reports whether c is part of a shell word (a character that keeps
// a token from being a standalone command name).
func isWordByte(c byte) bool {
	return c == '_' || c == '-' || c == '.' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// containsCommand reports whether cmd appears as a standalone word in s.
func containsCommand(s, cmd string) bool {
	for {
		idx := strings.Index(s, cmd)
		if idx < 0 {
			return false
		}
		beforeOK := idx == 0 || !isWordByte(s[idx-1])
		after := idx + len(cmd)
		afterOK := after == len(s) || !isWordByte(s[after])
		if beforeOK && afterOK {
			return true
		}
		s = s[idx+1:]
	}
}

// splitCompound splits a command into segments on the shell compound operators
// "&&", ";", "||" and "|". Empty segments are dropped.
func splitCompound(s string) []string {
	var segments []string
	start := 0
	i := 0
	for i < len(s) {
		switch {
		case i+1 < len(s) && s[i] == '&' && s[i+1] == '&':
			segments = append(segments, s[start:i])
			i += 2
			start = i
		case i+1 < len(s) && s[i] == '|' && s[i+1] == '|':
			segments = append(segments, s[start:i])
			i += 2
			start = i
		case s[i] == '|':
			segments = append(segments, s[start:i])
			i++
			start = i
		case s[i] == ';':
			segments = append(segments, s[start:i])
			i++
			start = i
		default:
			i++
		}
	}
	segments = append(segments, s[start:])

	var out []string
	for _, seg := range segments {
		if strings.TrimSpace(seg) != "" {
			out = append(out, seg)
		}
	}
	return out
}

// stripEnvAndChaining removes leading wrappers (sudo, command, env, nice,
// nohup, timeout), VAR=value assignments, and "cd X &&" / "cd X ;" prefixes so
// the actual command can be inspected.
func stripEnvAndChaining(command string) string {
	s := command
	for {
		t := strings.TrimSpace(s)
		if t == "" {
			return t
		}
		if next, ok := stripLeadingCd(t); ok {
			s = next
			continue
		}
		if next, ok := stripLeadingAssign(t); ok {
			s = next
			continue
		}
		if next, ok := stripLeadingWrapper(t); ok {
			s = next
			continue
		}
		return t
	}
}

// stripLeadingCd removes a leading "cd [dir] &&" or "cd [dir] ;" chain.
func stripLeadingCd(t string) (string, bool) {
	lower := strings.ToLower(t)
	if !strings.HasPrefix(lower, "cd") {
		return t, false
	}
	if len(t) == 2 {
		return t, false
	}
	if t[2] != ' ' && t[2] != '\t' {
		return t, false
	}
	rest := t[2:]
	idx := strings.IndexAny(rest, "&;")
	if idx < 0 {
		return t, false
	}
	op := rest[idx:]
	switch {
	case strings.HasPrefix(op, "&&"):
		return strings.TrimSpace(rest[idx+2:]), true
	case strings.HasPrefix(op, ";"):
		return strings.TrimSpace(rest[idx+1:]), true
	default:
		return t, false
	}
}

// stripLeadingAssign removes a single leading VAR=value assignment.
func stripLeadingAssign(t string) (string, bool) {
	eq := strings.Index(t, "=")
	if eq <= 0 {
		return t, false
	}
	if strings.ContainsAny(t[:eq], " \t/\\|&;") {
		return t, false
	}
	sp := strings.IndexAny(t[eq+1:], " \t")
	if sp < 0 {
		return "", true
	}
	return strings.TrimSpace(t[eq+1+sp+1:]), true
}

// stripAssigns removes every leading VAR=value assignment.
func stripAssigns(s string) string {
	for {
		next, ok := stripLeadingAssign(s)
		if !ok {
			return strings.TrimSpace(s)
		}
		s = next
	}
}

// stripLeadingWrapper removes one leading wrapper (sudo, command, env, nice,
// nohup, timeout N, timeout -s SIG N) if present.
func stripLeadingWrapper(t string) (string, bool) {
	lower := strings.ToLower(t)

	for _, w := range []string{"sudo", "command", "nice", "nohup"} {
		if lower == w {
			return "", true
		}
		if strings.HasPrefix(lower, w+" ") || strings.HasPrefix(lower, w+"\t") {
			rest := strings.TrimSpace(t[len(w):])
			if w == "command" && strings.HasPrefix(rest, "-") {
				// "command -v"/"command -V" is an inspection builtin.
				return t, false
			}
			return rest, true
		}
	}

	// env alone is read-only (printenv); "env [VAR=x ...] cmd" runs cmd.
	if lower == "env" {
		return t, false
	}
	if strings.HasPrefix(lower, "env ") || strings.HasPrefix(lower, "env\t") {
		rest := strings.TrimSpace(t[len("env"):])
		stripped := stripAssigns(rest)
		if stripped == "" {
			return "env", true
		}
		return stripped, true
	}

	if strings.HasPrefix(lower, "timeout ") || strings.HasPrefix(lower, "timeout\t") {
		return stripTimeout(t), true
	}

	return t, false
}

// stripTimeout strips the leading duration/signal arguments of "timeout".
func stripTimeout(t string) string {
	rest := strings.TrimSpace(t[len("timeout"):])
	if strings.HasPrefix(rest, "-s ") || strings.HasPrefix(rest, "-s\t") {
		rest = strings.TrimSpace(rest[2:])
		if sp := strings.IndexAny(rest, " \t"); sp >= 0 {
			rest = strings.TrimSpace(rest[sp+1:])
		} else {
			return ""
		}
	}
	if sp := strings.IndexAny(rest, " \t"); sp >= 0 {
		return strings.TrimSpace(rest[sp+1:])
	}
	return ""
}
