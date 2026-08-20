package main

import (
	"io/fs"
	"testing"
)

func TestFrontendAssetsContainIndex(t *testing.T) {
	assets := frontendAssets()
	if assets == nil {
		t.Fatal("frontendAssets returned nil")
	}
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		t.Fatalf("shared web index.html: %v", err)
	}
}
