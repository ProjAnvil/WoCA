package webdist

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
