// Package pluginrt implements woca's built-in capability plugins: tool
// bundles shipped inside the woca binary (embedded from the repo's
// woca-plugins/ tree) whose runtimes (uv-managed Python environments, the
// pinchtab browser server) are downloaded on demand under
// ~/.woca/plugins and managed by this package.
//
// Plugin kinds:
//
//	golang — the tool implementation is compiled into woca (this package);
//	         the woca-plugins/<name> directory carries the manifest + docs.
//	python — the plugin's scripts and dependency lock are embedded and run
//	         inside a uv-managed venv; only embedded scripts are executable.
package pluginrt

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"

	"github.com/projanvil/woca"
)

// Builtin is one plugin manifest (woca-plugins/<kind>/<name>/plugin.json).
type Builtin struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"` // "golang" | "python"
	Version     string   `json:"version,omitempty"`
	Description string   `json:"description,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	// Runtime names the downloadable runtime an install provisions:
	// "pinchtab" | "python" | "" (nothing to install).
	Runtime string `json:"runtime,omitempty"`
	// Scripts lists executable script paths (python plugins), relative to
	// the plugin directory.
	Scripts []string `json:"scripts,omitempty"`
	// Requirements is the dependency-lock path (python plugins).
	Requirements string `json:"requirements,omitempty"`

	dir string // plugin directory inside the embedded FS
}

// Dir returns the plugin's directory in the embedded filesystem
// (<kind>/<name>).
func (b Builtin) Dir() string { return b.dir }

// Builtins parses the embedded woca-plugins manifests in stable (kind, name)
// order. Parse failures are skipped: a broken manifest can only come from a
// bad build, and one unreadable plugin should not take the harness down.
func Builtins() []Builtin {
	var out []Builtin
	fsys := woca.PluginsFS()
	kinds, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return out
	}
	for _, k := range kinds {
		if !k.IsDir() {
			continue
		}
		entries, err := fs.ReadDir(fsys, k.Name())
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := k.Name() + "/" + e.Name()
			raw, err := fs.ReadFile(fsys, dir+"/plugin.json")
			if err != nil {
				continue
			}
			var b Builtin
			if err := json.Unmarshal(raw, &b); err != nil || b.Name == "" {
				continue
			}
			if b.Name == "" {
				b.Name = e.Name()
			}
			b.dir = dir
			out = append(out, b)
		}
	}
	slices.SortFunc(out, func(a, b Builtin) int {
		return cmp.Or(
			cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Name, b.Name),
		)
	})
	return out
}

// ByName looks a builtin plugin up by manifest name.
func ByName(name string) (Builtin, bool) {
	for _, b := range Builtins() {
		if b.Name == name {
			return b, true
		}
	}
	return Builtin{}, false
}

// PythonBuiltins returns the python-kind plugins (whose locks merge into the
// shared venv and whose scripts are materialized before execution).
func PythonBuiltins() []Builtin {
	var out []Builtin
	for _, b := range Builtins() {
		if b.Kind == "python" {
			out = append(out, b)
		}
	}
	return out
}

// readPluginFile reads one file from a plugin's embedded directory.
func readPluginFile(b Builtin, name string) ([]byte, error) {
	if b.dir == "" {
		return nil, fmt.Errorf("plugin %q has no directory", b.Name)
	}
	return fs.ReadFile(woca.PluginsFS(), b.dir+"/"+name)
}
