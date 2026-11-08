package sdk

// F5 alias contract guard (docs/superpowers/specs/2026-11-08-arch-cleanup-design.md,
// decision F5; audit woca-internal-docs/arch-review-2026-11-08.md, section F5).
//
// The sdk package is the official projection of internal/agent: every
// `X = agent.X` re-export below is public Go API (and carries the semantics
// the python/typescript bindings build on). This test pins the exact set so
// the external contract cannot drift silently:
//
//   - removing or renaming a binding fails the source diff,
//   - adding a new agent binding fails the source diff,
//   - repointing a binding at a different agent target, or changing its
//     declaration kind (type alias / const / var), fails the source diff,
//   - replacing a type alias with a distinct type of the same name fails
//     the reflect identity check,
//   - deleting a binding outright also breaks compilation of the anchors
//     at the bottom of this file.
//
// If this test fails because you deliberately changed the sdk surface:
// update the pinned lists (expectedAgentBindings and
// expectedLanguageBindings, plus typeAliasIdentities and the anchors
// below) in the SAME change, and call out the API change in sdk/README.md
// and the release notes. Do not "fix" the test by deleting it or by
// widening the contract without review.
//
// The same guard also pins the one non-agent alias, sdk.ChatModel =
// language.ChatModel (expectedLanguageBindings): it is plugin-facing
// public API exactly like the agent re-exports and must not drift
// silently either.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/woca/internal/agent"
)

// agentImportPath is the package every guarded agent binding must point at.
const agentImportPath = "github.com/projanvil/woca/internal/agent"

// languageImportPath is the package the guarded non-agent binding(s) point
// at (the model abstraction the plugin surface hands to wrappers).
const languageImportPath = "github.com/projanvil/langchain-golang/core/language"

// Declaration kinds, matching token.TYPE/CONST/VAR String() forms.
const (
	kindType  = "type"
	kindConst = "const"
	kindVar   = "var"
)

// bindingSpec describes one pinned `X = agent.Y` re-export.
type bindingSpec struct {
	kind   string // "type" (alias), "const", or "var"
	target string // identifier inside package agent
}

// expectedAgentBindings is the pinned sdk->agent contract: sdk name ->
// agent target. All bindings currently live in sdk/plugin.go.
var expectedAgentBindings = map[string]bindingSpec{
	// Plugin gating/transformation surface (type aliases).
	"ToolGuard":         {kindType, "ToolGuard"},
	"ToolAction":        {kindType, "ToolAction"},
	"ToolDecision":      {kindType, "ToolDecision"},
	"ToolCallInfo":      {kindType, "ToolCallInfo"},
	"ToolResultInfo":    {kindType, "ToolResultInfo"},
	"ToolPostDecision":  {kindType, "ToolPostDecision"},
	"ModelHook":         {kindType, "ModelHook"},
	"ModelTurn":         {kindType, "ModelTurn"},
	"ModelReply":        {kindType, "ModelReply"},
	"BeforeModelResult": {kindType, "BeforeModelResult"},
	"ModelWrapper":      {kindType, "ModelWrapper"},

	// Guard decision constants (values of ToolAction).
	"ToolAllow": {kindConst, "ToolAllow"},
	"ToolDeny":  {kindConst, "ToolDeny"},
	"ToolAsk":   {kindConst, "ToolAsk"},

	// Decision/feedback constructors (function values).
	"AllowTool":    {kindVar, "AllowTool"},
	"DenyTool":     {kindVar, "DenyTool"},
	"AskTool":      {kindVar, "AskTool"},
	"ToolFeedback": {kindVar, "ToolFeedback"},
}

// expectedLanguageBindings pins the sdk's non-agent re-exports: names
// aliased at the langchain-golang model abstraction rather than at
// internal/agent. Same rules as the agent map above.
var expectedLanguageBindings = map[string]bindingSpec{
	// The model interface wrappers receive and return (sdk/plugin.go).
	"ChatModel": {kindType, "ChatModel"},
}

// typeAliasIdentities anchors each pinned type alias at compile time and
// pins its runtime identity: an alias and its target must be the exact
// same reflect.Type. Instantiating typeOf[ToolGuard] here also makes alias
// removal/rename a build break in this file (the intended failure mode).
type aliasIdentity struct {
	name   string       // the sdk-side name
	target string       // how to name the aliased package in errors
	got    reflect.Type // the sdk-side name
	want   reflect.Type // the aliased package's target
}

var typeAliasIdentities = []aliasIdentity{
	{"ToolGuard", "agent.ToolGuard", typeOf[ToolGuard](), typeOf[agent.ToolGuard]()},
	{"ToolAction", "agent.ToolAction", typeOf[ToolAction](), typeOf[agent.ToolAction]()},
	{"ToolDecision", "agent.ToolDecision", typeOf[ToolDecision](), typeOf[agent.ToolDecision]()},
	{"ToolCallInfo", "agent.ToolCallInfo", typeOf[ToolCallInfo](), typeOf[agent.ToolCallInfo]()},
	{"ToolResultInfo", "agent.ToolResultInfo", typeOf[ToolResultInfo](), typeOf[agent.ToolResultInfo]()},
	{"ToolPostDecision", "agent.ToolPostDecision", typeOf[ToolPostDecision](), typeOf[agent.ToolPostDecision]()},
	{"ModelHook", "agent.ModelHook", typeOf[ModelHook](), typeOf[agent.ModelHook]()},
	{"ModelTurn", "agent.ModelTurn", typeOf[ModelTurn](), typeOf[agent.ModelTurn]()},
	{"ModelReply", "agent.ModelReply", typeOf[ModelReply](), typeOf[agent.ModelReply]()},
	{"BeforeModelResult", "agent.BeforeModelResult", typeOf[BeforeModelResult](), typeOf[agent.BeforeModelResult]()},
	{"ModelWrapper", "agent.ModelWrapper", typeOf[ModelWrapper](), typeOf[agent.ModelWrapper]()},
	{"ChatModel", "language.ChatModel", typeOf[ChatModel](), typeOf[language.ChatModel]()},
}

// typeOf returns the reflect.Type of T without constructing a value; it
// works uniformly for interfaces, structs, and named basic types.
func typeOf[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }

// Compile-time anchors for the const/var re-exports: removing or renaming
// any of them breaks the build of this guard, pointing here.
var _ = []any{ToolAllow, ToolDeny, ToolAsk, AllowTool, DenyTool, AskTool, ToolFeedback}

func TestAgentAliasContract(t *testing.T) {
	t.Run("pinned list matches sdk source", func(t *testing.T) {
		contracts := []struct {
			pkg      string // label in failure messages
			path     string // the aliased package's import path
			mapName  string // the pinned map to update
			expected map[string]bindingSpec
		}{
			{"agent", agentImportPath, "expectedAgentBindings", expectedAgentBindings},
			{"language", languageImportPath, "expectedLanguageBindings", expectedLanguageBindings},
		}
		for _, c := range contracts {
			t.Run(c.pkg, func(t *testing.T) {
				got := bindingsInSource(t, c.path)
				if len(got) == 0 {
					t.Fatalf("no %s bindings found in sdk sources; the extraction in this test is broken, not the contract", c.pkg)
				}

				var problems []string
				for _, name := range slices.Sorted(maps.Keys(c.expected)) {
					want := c.expected[name]
					actual, ok := got[name]
					if !ok {
						problems = append(problems, fmt.Sprintf(
							"- sdk.%s: binding removed or renamed; contract pins %s sdk.%s = %s.%s. "+
								"If the removal is intentional, delete its entry from %s "+
								"in sdk/alias_contract_test.go in the same change and call out the API break "+
								"in sdk/README.md and the release notes.",
							name, want.kind, name, c.pkg, want.target, c.mapName))
						continue
					}
					if actual != want {
						problems = append(problems, fmt.Sprintf(
							"- sdk.%s: repointed or rekinded; source now declares %s sdk.%s = %s.%s, "+
								"contract pins %s = %s.%s. Repointing an alias (or turning a type alias "+
								"into a const/var or vice versa) silently changes the public sdk API; "+
								"restore the pinned binding or update %s deliberately.",
							name, actual.kind, name, c.pkg, actual.target, want.kind, c.pkg, want.target, c.mapName))
					}
				}
				for _, name := range slices.Sorted(maps.Keys(got)) {
					if _, ok := c.expected[name]; !ok {
						problems = append(problems, fmt.Sprintf(
							"- sdk.%s: new %s binding %s sdk.%s = %s.%s is not in the pinned contract. "+
								"If consciously extending the public sdk surface, add the entry to "+
								"%s in sdk/alias_contract_test.go; otherwise drop the binding.",
							name, c.pkg, got[name].kind, name, c.pkg, got[name].target, c.mapName))
					}
				}

				if len(problems) > 0 {
					t.Fatalf("sdk %s alias contract drift (arch-cleanup spec F5).\n"+
						"The `X = %s.X` re-exports in this package are pinned public API; "+
						"resolve each item before committing:\n%s",
						c.pkg, c.pkg, strings.Join(problems, "\n"))
				}
			})
		}
	})

	t.Run("type aliases are identical runtime types", func(t *testing.T) {
		for _, id := range typeAliasIdentities {
			if id.got != id.want {
				t.Errorf("sdk.%s is not the same runtime type as %s (sdk: %v, target: %v); "+
					"it must stay a pure alias (`type %s = %s`). A distinct type with the "+
					"same name is a silent breaking change: restore the alias, or update this "+
					"guard AND sdk/README.md deliberately as a contract change.",
					id.name, id.target, id.got, id.want, id.name, id.target)
			}
		}
	})
}

// bindingsInSource parses this package's non-test .go files and returns
// every top-level `X = <pkg>.Y` binding for the given import path: type
// aliases, consts, and vars. It is a pure source scan (std-only, no
// network).
func bindingsInSource(t *testing.T, importPath string) map[string]bindingSpec {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed: cannot locate the sdk package sources")
	}
	dir := filepath.Dir(thisFile)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("cannot read sdk package dir %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	got := map[string]bindingSpec{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("sdk source %s no longer parses: %v", name, err)
		}

		// Identifiers in this file that qualify the aliased package
		// (handles renamed imports; default qualifier is the path base).
		pkgQualifiers := map[string]bool{}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if path != importPath {
				continue
			}
			qualifier := filepath.Base(path)
			if imp.Name != nil {
				qualifier = imp.Name.Name
			}
			pkgQualifiers[qualifier] = true
		}

		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if !s.Assign.IsValid() {
						continue // regular type declaration, not an alias
					}
					if sel, ok := s.Type.(*ast.SelectorExpr); ok {
						if id, ok := sel.X.(*ast.Ident); ok && pkgQualifiers[id.Name] {
							got[s.Name.Name] = bindingSpec{kind: kindType, target: sel.Sel.Name}
						}
					}
				case *ast.ValueSpec:
					if len(s.Names) != len(s.Values) {
						continue
					}
					for i, name := range s.Names {
						sel, ok := s.Values[i].(*ast.SelectorExpr)
						if !ok {
							continue
						}
						if id, ok := sel.X.(*ast.Ident); ok && pkgQualifiers[id.Name] {
							got[name.Name] = bindingSpec{kind: gd.Tok.String(), target: sel.Sel.Name}
						}
					}
				}
			}
		}
	}
	return got
}
