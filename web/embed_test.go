package webdist

import (
	"bytes"
	"io/fs"
	"regexp"
	"strings"
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

// TestDistFSRealBuildOrStub asserts the embedded FS is internally
// consistent: either it is the stub (no assets referenced), or every
// asset referenced by index.html exists in the FS.
func TestDistFSRealBuildOrStub(t *testing.T) {
	fsys := DistFS()
	index, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		t.Fatalf("index.html missing: %v", err)
	}
	if bytes.Contains(index, []byte("woca web UI not built")) {
		return // stub page, nothing referenced
	}
	for _, m := range regexp.MustCompile(`(?:src|href)="(/assets/[^"]+)"`).FindAllSubmatch(index, -1) {
		if _, err := fs.Stat(fsys, strings.TrimPrefix(string(m[1]), "/")); err != nil {
			t.Errorf("index.html references missing asset %s", m[1])
		}
	}
}
