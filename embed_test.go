package woca

import (
	"io/fs"
	"testing"
)

func TestDistFS(t *testing.T) {
	sub := DistFS()
	if sub == nil {
		t.Fatal("DistFS nil")
	}
	// The built SPA ships an index.html at the root of the sub-filesystem.
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		t.Fatalf("embedded index.html: %v", err)
	}
}

func TestPluginsFS(t *testing.T) {
	sub := PluginsFS()
	if sub == nil {
		t.Fatal("PluginsFS nil")
	}
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	if !names["golang"] || !names["python"] {
		t.Fatalf("builtin plugin dirs missing: %v", names)
	}
	// The manifests the settings UI lists are embedded.
	for _, m := range []string{"golang/browser/plugin.json", "python/pdf/plugin.json"} {
		if _, err := fs.Stat(sub, m); err != nil {
			t.Errorf("missing %s: %v", m, err)
		}
	}
}
