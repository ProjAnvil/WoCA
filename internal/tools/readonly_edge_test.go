package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIsReadOnlyCommandStrippingEdges drives the wrapper/assignment/cd
// stripping helpers through their fallback branches via the exported API.
func TestIsReadOnlyCommandStrippingEdges(t *testing.T) {
	cases := []struct {
		command string
		want    bool
	}{
		{"sudo", false},                 // wrapper alone strips to nothing
		{"env", true},                   // bare env is printenv
		{"env FOO=bar", true},           // assignment-only env stays "env"
		{"timeout 5", false},            // timeout without a command
		{"timeout -s KILL 5", false},    // timeout signal form without a command
		{"timeout -s KILL", false},      // signal flag without duration
		{"cd", false},                   // bare "cd" word
		{"cdfoo", false},                // prefix match must respect word chars
		{"cd /tmp & cmd", false},        // single "&" is not a stripped chain
		{"ls && sudo", false},           // empty segment after stripping is not read-only
		{"cat < in.txt", true},          // input redirection stays read-only
		{"sudo cat f | grep x", true},   // wrapper inside a pipe segment
		{"cd /a && cd /b && ls", true},  // repeated cd chains
		{"FOO=1 BAR=2 ls", true},        // multiple assignments
		{"FOO=bar", false},              // assignment without a command
	}
	for _, c := range cases {
		if got := IsReadOnlyCommand(c.command); got != c.want {
			t.Errorf("IsReadOnlyCommand(%q) = %v, want %v", c.command, got, c.want)
		}
	}
}

func TestIsDestructiveCommandEmptySegment(t *testing.T) {
	// A segment that strips to nothing (pure wrapper) is not destructive.
	if IsDestructiveCommand("ls && sudo") {
		t.Error("IsDestructiveCommand(ls && sudo) = true, want false")
	}
	if !IsDestructiveCommand("ls && sudo rm -rf x") {
		t.Error("IsDestructiveCommand(ls && sudo rm -rf x) = false, want true")
	}
}

func TestStripLeadingCdDirect(t *testing.T) {
	if next, ok := stripLeadingCd("cd /tmp && ls"); !ok || next != "ls" {
		t.Fatalf("stripLeadingCd(and) = %q, %v", next, ok)
	}
	if next, ok := stripLeadingCd("cd /tmp; ls"); !ok || next != "ls" {
		t.Fatalf("stripLeadingCd(semi) = %q, %v", next, ok)
	}
	if _, ok := stripLeadingCd("cd"); ok {
		t.Error(`stripLeadingCd("cd") reported a strip`)
	}
	if _, ok := stripLeadingCd("cdown"); ok {
		t.Error(`stripLeadingCd("cdown") reported a strip`)
	}
	if _, ok := stripLeadingCd("cd /tmp & cmd"); ok {
		t.Error(`stripLeadingCd("cd /tmp & cmd") reported a strip`)
	}
}

func TestStripLeadingAssignDirect(t *testing.T) {
	if next, ok := stripLeadingAssign("FOO=bar ls"); !ok || next != "ls" {
		t.Fatalf("stripLeadingAssign(with command) = %q, %v", next, ok)
	}
	if next, ok := stripLeadingAssign("FOO=bar"); !ok || next != "" {
		t.Fatalf("stripLeadingAssign(assignment only) = %q, %v", next, ok)
	}
	if _, ok := stripLeadingAssign("ls -la"); ok {
		t.Error("stripLeadingAssign on a plain command reported a strip")
	}
}

func TestStripLeadingWrapperDirect(t *testing.T) {
	if next, ok := stripLeadingWrapper("sudo ls"); !ok || next != "ls" {
		t.Fatalf("sudo wrapper = %q, %v", next, ok)
	}
	if next, ok := stripLeadingWrapper("sudo"); !ok || next != "" {
		t.Fatalf("bare sudo = %q, %v", next, ok)
	}
	if _, ok := stripLeadingWrapper("env"); ok {
		t.Error("bare env should not strip")
	}
	if next, ok := stripLeadingWrapper("env FOO=1 ls"); !ok || next != "ls" {
		t.Fatalf("env+assign wrapper = %q, %v", next, ok)
	}
	if next, ok := stripLeadingWrapper("env FOO=1"); !ok || next != "env" {
		t.Fatalf("env assignments only = %q, %v", next, ok)
	}
	if _, ok := stripLeadingWrapper("ls"); ok {
		t.Error("plain command should not strip")
	}
}

func TestStripTimeoutDirect(t *testing.T) {
	if got := stripTimeout("timeout 5 ls"); got != "ls" {
		t.Fatalf("stripTimeout(5 ls) = %q", got)
	}
	if got := stripTimeout("timeout -s KILL 5 ls"); got != "ls" {
		t.Fatalf("stripTimeout(-s KILL 5 ls) = %q", got)
	}
	if got := stripTimeout("timeout 5"); got != "" {
		t.Fatalf("stripTimeout(5) = %q, want empty", got)
	}
	if got := stripTimeout("timeout -s KILL 5"); got != "" {
		t.Fatalf("stripTimeout(-s KILL 5) = %q, want empty", got)
	}
}

func TestImageEdgeCases(t *testing.T) {
	if _, err := ProcessImage(filepath.Join(t.TempDir(), "gone.png")); err == nil {
		t.Fatal("ProcessImage on missing file = nil error")
	}

	// Height-driven long edge also triggers a resize.
	dir := t.TempDir()
	tall := filepath.Join(dir, "tall.png")
	writePNG(t, tall, 100, 3000)
	p, err := ProcessImage(tall)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Resized || p.MimeType != "image/jpeg" || p.Height > maxImageEdge {
		t.Fatalf("tall resize = %+v", p)
	}

	// Header-only image: DecodeConfig succeeds, full Decode fails.
	big := filepath.Join(dir, "truncated.png")
	writePNG(t, big, 3000, 3000)
	if err := os.Truncate(big, 33); err != nil { // signature + IHDR chunk only
		t.Fatal(err)
	}
	if _, err := ProcessImage(big); err == nil {
		t.Fatal("truncated image decode = nil error, want failure")
	}

	content, meta := FormatImageResult("tall.png", ImagePayload{Base64: "AAAA", MimeType: "image/jpeg", Width: 10, Height: 10, Resized: true})
	if !strings.Contains(content, "(resized to control token cost)") {
		t.Fatalf("resized note missing: %q", content)
	}
	if meta[MetaImageMime] != "image/jpeg" || meta[MetaImageBase64] != "AAAA" {
		t.Fatalf("metadata = %v", meta)
	}
}

func TestFileHistoryRestoreFailurePaths(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Redo whose target directory has become a regular file fails at MkdirAll.
	h := NewFileHistory()
	sub := filepath.Join(dir, "sub", "f.txt")
	if err := os.MkdirAll(filepath.Dir(sub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sub, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.Record(ctx, sub, nil, []byte("after"))
	if msg, err := h.Undo(ctx); err != nil || !strings.Contains(msg, "file removed") {
		t.Fatalf("undo creation = %q, %v", msg, err)
	}
	if err := os.Remove(filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub"), []byte("now a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Redo(ctx); err == nil || !strings.Contains(err.Error(), "restore") {
		t.Fatalf("redo over file-as-dir = %v, want restore error", err)
	}

	// Redo into a read-only directory fails at WriteFile.
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not block writes")
	}
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	h2 := NewFileHistory()
	target := filepath.Join(ro, "f.txt")
	if err := os.WriteFile(target, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	h2.Record(ctx, target, nil, []byte("content"))
	if _, err := h2.Undo(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(ro, 0o755) })
	if _, err := h2.Redo(ctx); err == nil || !strings.Contains(err.Error(), "restore") {
		t.Fatalf("redo into read-only dir = %v, want restore error", err)
	}
}
