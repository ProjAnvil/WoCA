package servefront

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// fakeObs records the Observability seam calls.
type fakeObs struct {
	installed  int
	wrapped    int
	shutdowned int
}

func (f *fakeObs) Install(context.Context) error { f.installed++; return nil }
func (f *fakeObs) Wrap(h http.Handler) http.Handler {
	f.wrapped++
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "obs-healthz")
			return
		}
		h.ServeHTTP(w, r)
	})
}
func (f *fakeObs) Shutdown(context.Context) error { f.shutdowned++; return nil }

func TestServeWithObservabilitySeam(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	obs := &fakeObs{}
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	if err := ServeWithObs(ctx, opts, "127.0.0.1:0", true, "", obs); err != nil {
		t.Fatalf("ServeWithObs = %v", err)
	}
	if obs.installed != 1 {
		t.Errorf("installed = %d, want 1", obs.installed)
	}
	if obs.wrapped < 2 {
		t.Errorf("wrapped = %d, want >=2 (startup placeholder + final handler)", obs.wrapped)
	}
	if obs.shutdowned != 1 {
		t.Errorf("shutdowned = %d, want 1", obs.shutdowned)
	}
}

func TestServeWithObsInstallError(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	failing := &failingObs{}
	if err := ServeWithObs(t.Context(), opts, "127.0.0.1:0", true, "", failing); err == nil {
		t.Fatal("expected install error to abort Serve")
	}
	if failing.obs.shutdowned != 0 {
		t.Errorf("shutdown must not run after a failed install")
	}
}

type failingObs struct{ obs fakeObs }

func (f *failingObs) Install(context.Context) error      { return errors.New("no provider") }
func (f *failingObs) Wrap(h http.Handler) http.Handler   { return h }
func (f *failingObs) Shutdown(ctx context.Context) error { return f.obs.Shutdown(ctx) }
