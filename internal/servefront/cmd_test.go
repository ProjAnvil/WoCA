package servefront

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/spf13/cobra"
)

func TestServeFunc(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	if err := Serve(ctx, opts, "127.0.0.1:0", true, ""); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestServeBadLogLevel(t *testing.T) {
	opts := agentbuild.Options{LogLevel: "bogus"}
	if err := Serve(t.Context(), opts, "127.0.0.1:0", true, ""); err == nil {
		t.Fatal("expected log-level error")
	}
}

func TestServeCmdNoBrowser(t *testing.T) {
	// Cover the serveCmd RunE wiring (flag → Serve) with an immediate cancel.
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")

	root := &cobra.Command{Use: "woca"}
	opts := &agentbuild.Options{}
	agentbuild.RegisterFlags(root.PersistentFlags(), opts)
	root.AddCommand(NewServeCmd(opts))
	root.SetArgs([]string{"serve", "--addr", "127.0.0.1:0", "--no-browser",
		"--provider", "openai", "--model", "m", "--api-key", "k", "--base-url", url,
		"--workdir", t.TempDir(), "--db", filepath.Join(home, "woca.db")})
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	root.SetContext(ctx)
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("serve cmd = %v", err)
	}
}
