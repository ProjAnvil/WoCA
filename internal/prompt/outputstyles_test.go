package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindStyle(t *testing.T) {
	for _, name := range []string{"", "default", "explanatory", "learning"} {
		if _, ok := FindStyle(name); !ok {
			t.Errorf("FindStyle(%q) not found", name)
		}
	}
	if _, ok := FindStyle("bogus"); ok {
		t.Error("bogus style should not resolve")
	}
}

func TestBuildWithStyleAppendsSection(t *testing.T) {
	dir := t.TempDir()
	base := BuildWithStyle(dir, "", "m", "default")
	expl := BuildWithStyle(dir, "", "m", "explanatory")
	if !strings.Contains(expl, "Output style: Explanatory") {
		t.Fatal("explanatory section missing")
	}
	if expl == base {
		t.Fatal("styles must differ")
	}
	if !strings.HasPrefix(expl, base) {
		t.Fatal("style section must append after the shared base (prompt-cache stable)")
	}
	learn := BuildWithStyle(dir, "", "m", "learning")
	if !strings.Contains(learn, "Output style: Learning") {
		t.Fatal("learning section missing")
	}
}

func TestSegmentCacheStableAndHits(t *testing.T) {
	resetSegmentCache()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("project rule: be terse"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := BuildWithStyle(dir, "extra", "model-x", "default")

	// Rebuilding with unchanged inputs hits the cache and stays identical;
	// without an extra prompt the styled build is exactly base + section.
	a0 := BuildWithStyle(dir, "", "model-x", "default")
	b0 := BuildWithStyle(dir, "", "model-x", "explanatory")
	if b0 != a0+"\n\n"+mustStyle(t, "explanatory").Text {
		t.Fatal("style switch must only append the style section")
	}
	c := BuildWithStyle(dir, "extra", "model-x", "default")
	if c != a {
		t.Fatal("rebuild not byte-identical")
	}
	hits, _ := CacheStats()
	if hits == 0 {
		t.Fatal("expected cache hits on rebuild")
	}

	// Touching the memory file (content change) invalidates its segment.
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("project rule: be terse, revised"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := BuildWithStyle(dir, "extra", "model-x", "default")
	if d == a {
		t.Fatal("memory file change must invalidate the segment")
	}
	if !strings.Contains(d, "revised") {
		t.Fatal("revised memory missing")
	}
}

func mustStyle(t *testing.T, name string) Style {
	t.Helper()
	st, ok := FindStyle(name)
	if !ok {
		t.Fatalf("style %q missing", name)
	}
	return st
}
