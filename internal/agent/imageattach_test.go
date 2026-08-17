package agent

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// TestReadImageAttachesContentBlock verifies the end-to-end vision path: the
// model Reads an image file and the resulting Tool message in the transcript
// carries an ImageBlock alongside its text content.
func TestReadImageAttachesContentBlock(t *testing.T) {
	dir := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	for x := 0; x < 10; x++ {
		for y := 0; y < 10; y++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	f, err := os.Create(filepath.Join(dir, "dot.png"))
	if err != nil {
		t.Fatal(err)
	}
	png.Encode(f, img)
	f.Close()

	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "r1", Name: "Read", Args: map[string]any{"file_path": "dot.png"}},
			}},
			messages.AI("it is red"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(dir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := a.Invoke(context.Background(), []messages.Message{messages.Human("what color?")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range out {
		if m.Role != messages.RoleTool || m.ToolCallID != "r1" {
			continue
		}
		if !strings.Contains(m.Content, "dot.png") {
			t.Fatalf("text portion = %q", m.Content)
		}
		if len(m.ContentBlocks) != 1 {
			t.Fatalf("expected 1 image block, got %d", len(m.ContentBlocks))
		}
		blk, ok := m.ContentBlocks[0].(messages.ImageBlock)
		if !ok {
			t.Fatalf("block type = %T", m.ContentBlocks[0])
		}
		if blk.MimeType != "image/png" || blk.Base64 == "" {
			t.Fatalf("block = %+v", blk)
		}
		return
	}
	t.Fatal("no tool result for the image read")
}
