package tools

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	coretools "github.com/projanvil/langchain-golang/core/tools"
)

const maxSearchResults = 200

const listDirDescription = `List the entries of a directory. Directories are suffixed with "/".`

type listDirArgs struct {
	Path string `json:"path,omitempty"`
}

func listDirTool(baseWorkdir string) coretools.Tool {
	t, err := coretools.FromFunc("ListDir", listDirDescription,
		func(ctx context.Context, a listDirArgs) (coretools.Result, error) {
			dir := resolvePath(baseWorkdir, a.Path)
			entries, err := os.ReadDir(dir)
			if err != nil {
				return coretools.Result{}, fmt.Errorf("list %s: %w", a.Path, err)
			}
			var b strings.Builder
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() {
					name += "/"
				}
				b.WriteString(name)
				b.WriteString("\n")
			}
			return coretools.Result{Content: b.String()}, nil
		})
	if err != nil {
		panic(err)
	}
	return t
}

const globDescription = `Find files whose paths match a glob pattern. A pattern with no "/" matches basenames at any depth (e.g. "*.go"). Supports "**", "*", "?", and character classes.`

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

func globTool(baseWorkdir string) coretools.Tool {
	t, err := coretools.FromFunc("Glob", globDescription,
		func(ctx context.Context, a globArgs) (coretools.Result, error) {
			base := resolvePath(baseWorkdir, a.Path)
			matches, err := walkMatches(base, a.Pattern, nil)
			if err != nil {
				return coretools.Result{}, err
			}
			return coretools.Result{Content: formatMatches(matches, "No files matched")}, nil
		})
	if err != nil {
		panic(err)
	}
	return t
}

const grepDescription = `Search file contents with a regular expression. Returns file:line:content for each match. Optionally filter files with a glob include pattern.`

type grepArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
	Include string `json:"include,omitempty"`
}

func grepTool(baseWorkdir string) coretools.Tool {
	t, err := coretools.FromFunc("Grep", grepDescription,
		func(ctx context.Context, a grepArgs) (coretools.Result, error) {
			re, err := regexp.Compile(a.Pattern)
			if err != nil {
				return coretools.Result{}, fmt.Errorf("invalid regexp %q: %w", a.Pattern, err)
			}
			base := resolvePath(baseWorkdir, a.Path)

			var include *regexp.Regexp
			if a.Include != "" {
				include, err = globToRegexp(a.Include)
				if err != nil {
					return coretools.Result{}, fmt.Errorf("invalid include glob %q: %w", a.Include, err)
				}
			}

			files, err := walkMatches(base, "**", include)
			if err != nil {
				return coretools.Result{}, err
			}

			var b strings.Builder
			count := 0
			for _, rel := range files {
				if count >= maxSearchResults {
					break
				}
				abs := filepath.Join(base, filepath.FromSlash(rel))
				matches, err := grepFile(abs, re)
				if err != nil {
					continue
				}
				for _, m := range matches {
					fmt.Fprintf(&b, "%s:%s\n", rel, m)
					count++
					if count >= maxSearchResults {
						break
					}
				}
			}
			if b.Len() == 0 {
				return coretools.Result{Content: "No matches found."}, nil
			}
			if count >= maxSearchResults {
				fmt.Fprintf(&b, "... (truncated at %d matches)\n", maxSearchResults)
			}
			return coretools.Result{Content: b.String()}, nil
		})
	if err != nil {
		panic(err)
	}
	return t
}

// walkMatches walks base and returns the slash-separated relative paths of
// files matching pattern (a glob). include optionally filters which paths are
// even considered (compiled glob regexp).
func walkMatches(base, pattern string, include *regexp.Regexp) ([]string, error) {
	matcher, err := globToRegexp(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid glob %q: %w", pattern, err)
	}
	// A pattern without "/" also matches basenames at any depth.
	var deep *regexp.Regexp
	if !strings.Contains(pattern, "/") {
		deep, _ = globToRegexp("**/" + pattern)
	}

	var out []string
	info, err := os.Stat(base)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", base, err)
	}
	if !info.IsDir() {
		rel := filepath.Base(base)
		if matchAny(matcher, deep, filepath.ToSlash(rel)) {
			out = append(out, rel)
		}
		return out, nil
	}

	err = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(base, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if include != nil && !include.MatchString(rel) {
			return nil
		}
		if matchAny(matcher, deep, rel) {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", base, err)
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

// grepFile returns "line:content" strings for each line matching re.
func grepFile(path string, re *regexp.Regexp) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if re.MatchString(line) {
			out = append(out, fmt.Sprintf("%d:%s", lineNo, line))
		}
	}
	return out, scanner.Err()
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
