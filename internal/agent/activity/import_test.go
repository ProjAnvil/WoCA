package activity

import (
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
// (arch-cleanup spec F1). Importing it from here — production file or test
// file — would create an import cycle (internal/agent imports this package),
// or worse, quietly re-couple the extracted seam to the god package. The
// compiler rejects the cycle in same-package files, but an external
// activity_test file importing internal/agent would compile and rot the
// architecture; this guard makes both impossible to miss.
const agentImportPath = "github.com/projanvil/woca/internal/agent"

// TestPackageNeverImportsAgent fails if any .go file in this package —
// production or test, internal or external — imports internal/agent.
func TestPackageNeverImportsAgent(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed: cannot locate the activity package sources")
	}
	dir := filepath.Dir(thisFile)

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
		t.Fatalf("activity package must not import internal/agent (spec F1, dependency "+
			"inversion): the seam's manager surfaces arrive as interfaces injected by "+
			"the agent package. Offending files:\n%s",
			strings.Join(offenders, "\n"))
	}
}
