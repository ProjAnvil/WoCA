package tools

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	// Decode-only registrations for the formats the Read tool attaches.
	_ "image/gif"
	_ "image/png"
	"os"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Image input support: Read on an image file attaches it to the tool result
// as a vision content block instead of dumping bytes as text. Payloads are
// validated by magic bytes, capped at 5 MB, and downscaled on the long edge
// (1568 px) to control token cost.

const (
	maxImageBytes = 5 << 20
	maxImageEdge  = 1568
)

var imageExts = map[string]bool{
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".gif":  true,
	".webp": true,
}

// IsImagePath reports whether the extension marks a file the Read tool can
// attach as an image block.
func IsImagePath(path string) bool {
	return imageExts[lowerExt(path)]
}

func lowerExt(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			return toLower(path[i:])
		}
		if path[i] == '/' || path[i] == '\\' {
			break
		}
	}
	return ""
}

func toLower(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'A' && c <= 'Z' {
			out[i] = c + ('a' - 'A')
		}
	}
	return string(out)
}

var mimeByFormat = map[string]string{
	"png":  "image/png",
	"jpeg": "image/jpeg",
	"gif":  "image/gif",
	"webp": "image/webp",
}

// ImagePayload is a processed image ready for an ImageBlock.
type ImagePayload struct {
	Base64   string
	MimeType string
	Width    int
	Height   int
	Resized  bool
}

// ProcessImage validates, optionally downscales, and base64-encodes an image
// file. Non-image or oversized inputs return an error the tool surfaces.
func ProcessImage(path string) (ImagePayload, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ImagePayload{}, err
	}
	if len(data) > maxImageBytes {
		return ImagePayload{}, fmt.Errorf("image is %d bytes, over the %d-byte limit", len(data), maxImageBytes)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return ImagePayload{}, fmt.Errorf("not a decodable image (%v)", err)
	}
	mime, ok := mimeByFormat[format]
	if !ok {
		return ImagePayload{}, fmt.Errorf("unsupported image format %q", format)
	}

	long := cfg.Width
	if cfg.Height > long {
		long = cfg.Height
	}
	if long <= maxImageEdge {
		return ImagePayload{
			Base64:   base64.StdEncoding.EncodeToString(data),
			MimeType: mime,
			Width:    cfg.Width,
			Height:   cfg.Height,
		}, nil
	}

	// Downscale on the long edge and re-encode as JPEG.
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return ImagePayload{}, err
	}
	scale := float64(maxImageEdge) / float64(long)
	w := max(1, int(float64(cfg.Width)*scale))
	h := max(1, int(float64(cfg.Height)*scale))
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
		return ImagePayload{}, err
	}
	return ImagePayload{
		Base64:   base64.StdEncoding.EncodeToString(buf.Bytes()),
		MimeType: "image/jpeg",
		Width:    w,
		Height:   h,
		Resized:  true,
	}, nil
}

// Result metadata keys the agent uses to attach the vision block.
const (
	MetaImageBase64 = "image_base64"
	MetaImageMime   = "image_mime"
)

// FormatImageResult renders the text portion of an image-bearing tool result
// and its metadata.
func FormatImageResult(path string, p ImagePayload) (content string, meta map[string]any) {
	kb := len(p.Base64) * 3 / 4 / 1024
	note := ""
	if p.Resized {
		note = " (resized to control token cost)"
	}
	content = fmt.Sprintf("Read image %s — %dx%d %s, ~%d KB%s. The image is attached to this tool result as an image block; describe and reason about it visually.",
		path, p.Width, p.Height, p.MimeType, kb, note)
	meta = map[string]any{
		MetaImageBase64: p.Base64,
		MetaImageMime:   p.MimeType,
	}
	return content, meta
}
