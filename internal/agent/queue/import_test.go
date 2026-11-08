package queue

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// agentImportPath is the parent package this subpackage was extracted from
// (queue-extract spec). Importing it from here — production file or test
// file — would create an import cycle (internal/agent imports this package),
// or worse, quietly re-couple the extracted buffer to the god package. The
// compiler rejects the cycle in same-package files, but an external
// queue_test file importing internal/agent would compile and rot the
// architecture; this guard makes both impossible to miss.
const agentImportPath = "github.com/projanvil/woca/internal/agent"

// pkgImportPath is this package's own import path, used to relocate the
// sources on disk when -trimpath has rewritten runtime.Caller paths (see
// pkgSourceDir).
const pkgImportPath = "github.com/projanvil/woca/internal/agent/queue"

// TestPackageNeverImportsAgent fails if any .go file in this package —
// production or test, internal or external — imports internal/agent.
func TestPackageNeverImportsAgent(t *testing.T) {
	dir := pkgSourceDir(t, pkgImportPath)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("cannot read package dir %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	offenders := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s no longer parses: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if path == agentImportPath {
				offenders = append(offenders, fmt.Sprintf("%s imports %s", name, path))
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("queue package must not import internal/agent (queue-extract spec, "+
			"dependency inversion): the buffer is mechanics only; policy (released "+
			"predicate, Quiet decision, observer) is injected or composed by the "+
			"agent package. Offending files:\n%s",
			strings.Join(offenders, "\n"))
	}
}

// pkgSourceDir resolves this package's source directory on disk, given the
// package's import path.
//
// The runtime.Caller-derived directory of this file is the primary source of
// truth, but `go test -trimpath` rewrites recorded file paths into
// module-relative paths ("github.com/projanvil/woca/internal/agent/queue/...")
// that do not exist on disk. When that happens, walk up from the working
// directory (go test runs test binaries inside the package tree) to the go.mod
// declaring this module and resolve the directory for pkgImportPath from its
// module path. Every resolution failure is fatal: silently skipping the scan
// would defeat the guard this file exists for.
func pkgSourceDir(t *testing.T, pkgImportPath string) string {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed: cannot locate %s sources", pkgImportPath)
	}
	if dir := filepath.Dir(thisFile); isDir(dir) {
		return dir
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("cannot locate %s sources: runtime.Caller path %q is not on disk "+
			"(-trimpath build) and os.Getwd failed: %v", pkgImportPath, thisFile, err)
	}
	root, mod, err := moduleRoot(wd)
	if err != nil {
		t.Fatalf("cannot locate %s sources: runtime.Caller path %q is not on disk "+
			"(-trimpath build) and the module lookup failed: %v", pkgImportPath, thisFile, err)
	}
	rel, ok := strings.CutPrefix(pkgImportPath, mod+"/")
	if !ok {
		t.Fatalf("cannot locate %s sources: package is not inside module %s (go.mod at %s)",
			pkgImportPath, mod, root)
	}
	dir := filepath.Join(root, rel)
	if !isDir(dir) {
		t.Fatalf("cannot locate %s sources: resolved %s from the go.mod at %s, "+
			"but it is not a directory", pkgImportPath, dir, root)
	}
	return dir
}

// isDir reports whether path is an existing directory.
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// moduleRoot walks up from startDir to the nearest go.mod and returns its
// directory and its declared module path.
func moduleRoot(startDir string) (root, mod string, err error) {
	dir := startDir
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			if mod := moduleDirective(data); mod != "" {
				return dir, mod, nil
			}
			return "", "", fmt.Errorf("go.mod at %s has no module directive", dir)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", fmt.Errorf("no go.mod found at or above %s", startDir)
		}
		dir = parent
	}
}

// moduleDirective extracts the module path from go.mod contents (surrounding
// quotes trimmed); "" when the directive is absent.
func moduleDirective(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.Trim(strings.TrimSpace(after), `"`)
		}
	}
	return ""
}
