package tools

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePNG writes a solid-color PNG of the given size.
func writePNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{R: 200, G: 30, B: 30, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func TestIsImagePath(t *testing.T) {
	for p, want := range map[string]bool{
		"a.png": true, "b.JPG": true, "c.jpeg": true, "d.gif": true, "e.webp": true,
		"f.txt": false, "g.pngx": false, "h": false, "dir/png/file": false,
	} {
		if got := IsImagePath(p); got != want {
			t.Errorf("IsImagePath(%q) = %v", p, got)
		}
	}
}

func TestProcessImagePassthrough(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "small.png")
	writePNG(t, p, 64, 32)

	payload, err := ProcessImage(p)
	if err != nil {
		t.Fatal(err)
	}
	if payload.MimeType != "image/png" || payload.Width != 64 || payload.Height != 32 || payload.Resized {
		t.Fatalf("payload = %+v", payload)
	}
	raw, _ := os.ReadFile(p)
	if payload.Base64 != base64.StdEncoding.EncodeToString(raw) {
		t.Fatal("small images must be passed through byte-for-byte")
	}
}

func TestProcessImageResizesLongEdge(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.png")
	writePNG(t, p, 3000, 2000)

	payload, err := ProcessImage(p)
	if err != nil {
		t.Fatal(err)
	}
	if !payload.Resized {
		t.Fatal("large image should be resized")
	}
	if payload.MimeType != "image/jpeg" {
		t.Fatalf("resized images re-encode as jpeg, got %s", payload.MimeType)
	}
	long := payload.Width
	if payload.Height > long {
		long = payload.Height
	}
	if long != maxImageEdge && long != maxImageEdge-1 {
		t.Fatalf("long edge = %d, want ~%d", long, maxImageEdge)
	}
}

func TestProcessImageRejectsNonImage(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fake.png")
	if err := os.WriteFile(p, []byte("definitely not a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ProcessImage(p); err == nil {
		t.Fatal("expected a decode error")
	}
}

func TestProcessImageRejectsOversized(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "huge.png")
	// A valid PNG header followed by junk large enough to exceed the cap.
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	png.Encode(&buf, img) // header magic for DecodeConfig
	buf.Write(bytes.Repeat([]byte{0}, maxImageBytes))
	os.WriteFile(p, buf.Bytes(), 0o644)
	if _, err := ProcessImage(p); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}

func TestReadToolAttachesImage(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, filepath.Join(dir, "shot.png"), 100, 50)
	reg := NewRegistry(dir)
	tool, _ := reg.Get("Read")
	res, err := tool.Invoke(t.Context(), map[string]any{"file_path": "shot.png"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "shot.png") || !strings.Contains(res.Content, "image/png") {
		t.Fatalf("content = %q", res.Content)
	}
	b64, _ := res.Metadata[MetaImageBase64].(string)
	mime, _ := res.Metadata[MetaImageMime].(string)
	if b64 == "" || mime != "image/png" {
		t.Fatalf("metadata missing image: %v", res.Metadata)
	}
	if _, err := base64.StdEncoding.DecodeString(b64); err != nil {
		t.Fatalf("base64 invalid: %v", err)
	}
}

func TestReadToolNonImageUnchanged(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("plain text"), 0o644)
	reg := NewRegistry(dir)
	tool, _ := reg.Get("Read")
	res, err := tool.Invoke(t.Context(), map[string]any{"file_path": "a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Metadata != nil || !strings.Contains(res.Content, "plain text") {
		t.Fatalf("text read changed: %+v", res)
	}
}
