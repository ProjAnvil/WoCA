package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/platform/executor"
)

const maxSearchResults = 200

const listDirDescription = `List the entries of a directory. Directories are suffixed with "/".`

type listDirArgs struct {
	Path string `json:"path,omitempty"`
}

// listDirTool lists one directory through the resolved executor: a Glob
// of dir/* plus a Stat per entry distinguishes files from directories
// (the Executor interface has no ReadDir, design §10.1 C-2 keeps it
// frozen), so sandboxed runs list the sandbox filesystem instead of the
// host.
func listDirTool(d *Dir, execFor func(context.Context) executor.Executor) coretools.Tool {
	t := MustTool(coretools.FromFunc("ListDir", listDirDescription,
		func(ctx context.Context, a listDirArgs) (coretools.Result, error) {
			dir := resolvePath(d.For(ctx), a.Path)
			x := execFor(ctx)
			info, err := x.Stat(ctx, dir)
			if err != nil {
				return coretools.Result{}, fmt.Errorf("list %s: %w", a.Path, err)
			}
			if !info.IsDir() {
				return coretools.Result{}, fmt.Errorf("list %s: not a directory", a.Path)
			}
			entries, err := x.Glob(ctx, filepath.Join(dir, "*"))
			if err != nil {
				return coretools.Result{}, fmt.Errorf("list %s: %w", a.Path, err)
			}
			var b strings.Builder
			for _, name := range sortedEntries(listEntryNames(ctx, x, entries)) {
				b.WriteString(name)
				b.WriteString("\n")
			}
			return coretools.Result{Content: b.String()}, nil
		}))
	return t
}

// listEntryNames maps one directory's globbed entries to display names,
// suffixing directories with "/" via a Stat per entry.
func listEntryNames(ctx context.Context, x executor.Executor, entries []string) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		fi, err := x.Stat(ctx, e)
		if err != nil {
			continue
		}
		name := filepath.Base(e)
		if fi.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	return names
}

const globDescription = `Find files whose paths match a glob pattern. A pattern with no "/" matches basenames at any depth (e.g. "*.go"). Supports "**", "*", "?", and character classes.`

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

func globTool(d *Dir, execFor func(context.Context) executor.Executor) coretools.Tool {
	t := MustTool(coretools.FromFunc("Glob", globDescription,
		func(ctx context.Context, a globArgs) (coretools.Result, error) {
			base := resolvePath(d.For(ctx), a.Path)
			matches, err := walkMatches(ctx, execFor, base, a.Pattern, nil)
			if err != nil {
				return coretools.Result{}, err
			}
			return coretools.Result{Content: formatMatches(matches, "No files matched")}, nil
		}))
	return t
}

const grepDescription = `Search file contents with a regular expression. Returns file:line:content for each match. Optionally filter files with a glob include pattern.`

type grepArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
	Include string `json:"include,omitempty"`
}

// grepTool runs the content search through the executor's Grep and
// post-filters by the include glob on the relative paths (the executor
// interface carries no include parameter; the filter is path-shape only,
// so applying it client-side is equivalent).
func grepTool(d *Dir, execFor func(context.Context) executor.Executor) coretools.Tool {
	t := MustTool(coretools.FromFunc("Grep", grepDescription,
		func(ctx context.Context, a grepArgs) (coretools.Result, error) {
			re, err := regexp.Compile(a.Pattern)
			if err != nil {
				return coretools.Result{}, fmt.Errorf("invalid regexp %q: %w", a.Pattern, err)
			}
			base := resolvePath(d.For(ctx), a.Path)

			var include *regexp.Regexp
			if a.Include != "" {
				include, err = globToRegexp(a.Include)
				if err != nil {
					return coretools.Result{}, fmt.Errorf("invalid include glob %q: %w", a.Include, err)
				}
			}

			matches, err := execFor(ctx).Grep(ctx, base, re)
			if err != nil {
				return coretools.Result{}, fmt.Errorf("grep %s: %w", a.Path, err)
			}

			var b strings.Builder
			count := 0
			for _, m := range matches {
				if count >= maxSearchResults {
					break
				}
				rel, rerr := filepath.Rel(base, m.Path)
				if rerr != nil {
					continue
				}
				rel = filepath.ToSlash(rel)
				if include != nil && !include.MatchString(rel) {
					continue
				}
				fmt.Fprintf(&b, "%s:%d:%s\n", rel, m.Line, m.Text)
				count++
			}
			if b.Len() == 0 {
				return coretools.Result{Content: "No matches found."}, nil
			}
			if count >= maxSearchResults {
				fmt.Fprintf(&b, "... (truncated at %d matches)\n", maxSearchResults)
			}
			return coretools.Result{Content: b.String()}, nil
		}))
	return t
}

// walkMatches enumerates the files under base through the executor and
// returns the slash-separated relative paths matching pattern (a glob).
// include optionally filters which paths are even considered (compiled
// glob regexp). The Executor interface is frozen (no tree walk), so the
// enumeration is a recursive Glob+Stat combination — one level per call —
// which reproduces the previous filepath.WalkDir file list.
func walkMatches(ctx context.Context, execFor func(context.Context) executor.Executor, base, pattern string, include *regexp.Regexp) ([]string, error) {
	matcher, err := globToRegexp(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid glob %q: %w", pattern, err)
	}
	// A pattern without "/" also matches basenames at any depth.
	var deep *regexp.Regexp
	if !strings.Contains(pattern, "/") {
		deep, _ = globToRegexp("**/" + pattern)
	}

	x := execFor(ctx)
	info, err := x.Stat(ctx, base)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", base, err)
	}
	if !info.IsDir() {
		rel := filepath.Base(base)
		if matchAny(matcher, deep, filepath.ToSlash(rel)) {
			return []string{rel}, nil
		}
		return nil, nil
	}

	var files []string
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := x.Glob(ctx, filepath.Join(dir, "*"))
		if err != nil {
			return // skip unreadable directories (WalkDir parity)
		}
		for _, e := range entries {
			fi, err := x.Stat(ctx, e)
			if err != nil {
				continue // skip unreadable entries (WalkDir parity)
			}
			if fi.IsDir() {
				walk(e)
				continue
			}
			files = append(files, e)
		}
	}
	walk(base)
	slices.Sort(files)

	var out []string
	for _, f := range files {
		rel, rerr := filepath.Rel(base, f)
		if rerr != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		if include != nil && !include.MatchString(rel) {
			continue
		}
		if matchAny(matcher, deep, rel) {
			out = append(out, rel)
		}
	}
	return out, nil
}

func matchAny(m, deep *regexp.Regexp, rel string) bool {
	if m.MatchString(rel) {
		return true
	}
	return deep != nil && deep.MatchString(rel)
}

func formatMatches(matches []string, empty string) string {
	if len(matches) == 0 {
		return empty
	}
	if len(matches) > maxSearchResults {
		matches = matches[:maxSearchResults]
	}
	return strings.Join(matches, "\n") + "\n"
}

// globToRegexp compiles a glob (with "**", "*", "?", "[...]") into a regexp
// anchored to the full path.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			j := i + 1
			for j < len(pattern) && pattern[j] != ']' {
				j++
			}
			if j < len(pattern) {
				b.WriteString(pattern[i : j+1])
				i = j
			} else {
				b.WriteString(`\[`)
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
