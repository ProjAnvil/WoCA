package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/projanvil/woca/internal/relayclient"
	"github.com/projanvil/woca/internal/settings"
)

// captureStdout redirects os.Stdout (printRelayStatus uses fmt.Printf
// directly) while fn runs and returns whatever was written.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	_ = w.Close()
	os.Stdout = old
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), runErr
}

// isolateRelayEnv isolates the environment and clears the relay env
// overrides so persisted settings (not the developer's shell) are resolved.
func isolateRelayEnv(t *testing.T) string {
	t.Helper()
	t.Setenv(relayclient.EnvRelayURL, "")
	t.Setenv(relayclient.EnvRelayKey, "")
	return isolateEnv(t)
}

// relayConfig resolves the persisted relay configuration of the isolated DB.
func relayConfig(t *testing.T) (url, key string) {
	t.Helper()
	st, err := settings.Open(os.Getenv("WOCA_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	return relayclient.ResolveConfig(st)
}

func TestRelayCmdHelp(t *testing.T) {
	if err := execCmd(t, "relay"); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "relay", "--help"); err != nil {
		t.Fatal(err)
	}
}

func TestRelayConnect(t *testing.T) {
	t.Run("requires url and key", func(t *testing.T) {
		isolateRelayEnv(t)
		for _, args := range [][]string{
			{"relay", "connect"},
			{"relay", "connect", "--url", "https://relay.example.com"},
			{"relay", "connect", "--key", "wcr_abc"},
		} {
			err := execCmd(t, args...)
			if err == nil || !strings.Contains(err.Error(), "requires --url and --key") {
				t.Fatalf("args %v: err = %v", args, err)
			}
		}
	})

	t.Run("rejects bad key prefix", func(t *testing.T) {
		isolateRelayEnv(t)
		err := execCmd(t, "relay", "connect", "--url", "https://relay.example.com", "--key", "nope")
		if err == nil || !strings.Contains(err.Error(), "relay keys start with") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("persists configuration", func(t *testing.T) {
		isolateRelayEnv(t)
		out, err := captureStdout(t, func() error {
			return execCmd(t, "relay", "connect", "--url", "https://relay.example.com/", "--key", "wcr_secret")
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "relay configured") {
			t.Fatalf("out = %q", out)
		}
		url, key := relayConfig(t)
		// SaveConfig canonicalizes trailing slashes.
		if url != "https://relay.example.com" || key != "wcr_secret" {
			t.Fatalf("persisted = %q %q", url, key)
		}
	})

	t.Run("env flag defaults", func(t *testing.T) {
		isolateRelayEnv(t)
		// Flag defaults are read from the environment at command
		// construction (execCmd builds a fresh root each call).
		t.Setenv(relayclient.EnvRelayURL, "https://env-relay.example.com")
		t.Setenv(relayclient.EnvRelayKey, "wcr_env")
		if err := execCmd(t, "relay", "connect"); err != nil {
			t.Fatal(err)
		}
		url, key := relayConfig(t)
		if url != "https://env-relay.example.com" || key != "wcr_env" {
			t.Fatalf("persisted = %q %q", url, key)
		}
	})

	t.Run("settings open error", func(t *testing.T) {
		home := isolateRelayEnv(t)
		blocker := filepath.Join(home, "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := execCmd(t, "--db", filepath.Join(blocker, "woca.db"),
			"relay", "connect", "--url", "https://relay.example.com", "--key", "wcr_x")
		if err == nil {
			t.Fatal("expected settings open error")
		}
	})

	t.Run("settings disabled", func(t *testing.T) {
		isolateRelayEnv(t)
		err := execCmd(t, "--db", "none",
			"relay", "connect", "--url", "https://relay.example.com", "--key", "wcr_x")
		if err == nil || !strings.Contains(err.Error(), "settings database disabled") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRelayStatusLive(t *testing.T) {
	isolateRelayEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/relay/status" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"configured":true,"connected":true,"key_prefix":"wcr_ab","generation":"gen-1","revoked":false,"last_error":"","served":7}`))
	}))
	t.Cleanup(srv.Close)

	out, err := captureStdout(t, func() error {
		return execCmd(t, "relay", "status", "--server", srv.URL)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"configured: true", "key_prefix: wcr_ab", "connected: true",
		"generation: gen-1", "revoked: false", "served: 7",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("out missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "last_error") {
		t.Fatalf("empty last_error should be omitted: %q", out)
	}
}

func TestRelayStatusFallback(t *testing.T) {
	const down = "http://127.0.0.1:1"

	t.Run("unconfigured", func(t *testing.T) {
		isolateRelayEnv(t)
		out, err := captureStdout(t, func() error {
			return execCmd(t, "relay", "status", "--server", down)
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "configured: false") || !strings.Contains(out, "connected: false") {
			t.Fatalf("out = %q", out)
		}
	})

	t.Run("configured persisted fallback", func(t *testing.T) {
		isolateRelayEnv(t)
		st, err := settings.Open(os.Getenv("WOCA_DB"))
		if err != nil {
			t.Fatal(err)
		}
		if err := relayclient.SaveConfig(st, "https://relay.example.com", "wcr_persisted"); err != nil {
			t.Fatal(err)
		}
		st.Close()

		out, err := captureStdout(t, func() error {
			return execCmd(t, "relay", "status", "--server", down)
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "configured: true") || !strings.Contains(out, "key_prefix: wcr_pers") {
			t.Fatalf("out = %q", out)
		}
	})

	t.Run("settings open error", func(t *testing.T) {
		home := isolateRelayEnv(t)
		blocker := filepath.Join(home, "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := execCmd(t, "--db", filepath.Join(blocker, "woca.db"),
			"relay", "status", "--server", down)
		if err == nil {
			t.Fatal("expected settings open error")
		}
	})
}

func TestRelayDisconnect(t *testing.T) {
	t.Run("clears config and stops live tunnel", func(t *testing.T) {
		isolateRelayEnv(t)
		st, err := settings.Open(os.Getenv("WOCA_DB"))
		if err != nil {
			t.Fatal(err)
		}
		if err := relayclient.SaveConfig(st, "https://relay.example.com", "wcr_x"); err != nil {
			t.Fatal(err)
		}
		st.Close()

		var posts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/api/relay/disconnect" {
				posts.Add(1)
			}
		}))
		t.Cleanup(srv.Close)

		out, err := captureStdout(t, func() error {
			return execCmd(t, "relay", "disconnect", "--server", srv.URL)
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "relay disconnected") {
			t.Fatalf("out = %q", out)
		}
		if posts.Load() != 1 {
			t.Fatalf("live disconnect POSTs = %d", posts.Load())
		}
		if url, key := relayConfig(t); url != "" || key != "" {
			t.Fatalf("config not cleared: %q %q", url, key)
		}
	})

	t.Run("serve down still clears", func(t *testing.T) {
		isolateRelayEnv(t)
		st, err := settings.Open(os.Getenv("WOCA_DB"))
		if err != nil {
			t.Fatal(err)
		}
		if err := relayclient.SaveConfig(st, "https://relay.example.com", "wcr_x"); err != nil {
			t.Fatal(err)
		}
		st.Close()

		out, err := captureStdout(t, func() error {
			return execCmd(t, "relay", "disconnect", "--server", "http://127.0.0.1:1")
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "relay disconnected") {
			t.Fatalf("out = %q", out)
		}
		if url, key := relayConfig(t); url != "" || key != "" {
			t.Fatalf("config not cleared: %q %q", url, key)
		}
	})

	t.Run("unparseable server URL is best effort", func(t *testing.T) {
		isolateRelayEnv(t)
		// http.NewRequest fails on a control-character URL; disconnect
		// treats the live stop as best effort and still succeeds.
		if err := execCmd(t, "relay", "disconnect", "--server", "http://\x7f.invalid"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("settings open error", func(t *testing.T) {
		home := isolateRelayEnv(t)
		blocker := filepath.Join(home, "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := execCmd(t, "--db", filepath.Join(blocker, "woca.db"), "relay", "disconnect")
		if err == nil {
			t.Fatal("expected settings open error")
		}
	})
}

func TestFetchRelayStatus(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"configured":true,"connected":true,"served":3}`))
		}))
		t.Cleanup(srv.Close)
		st, err := fetchRelayStatus(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Configured || !st.Connected || st.Served != 3 {
			t.Fatalf("status = %+v", st)
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		if _, err := fetchRelayStatus("http://127.0.0.1:1"); err == nil {
			t.Fatal("expected dial error")
		}
	})

	t.Run("non-200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)
		_, err := fetchRelayStatus(srv.URL)
		if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("bad json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("not-json"))
		}))
		t.Cleanup(srv.Close)
		if _, err := fetchRelayStatus(srv.URL); err == nil {
			t.Fatal("expected decode error")
		}
	})
}

func TestPrintRelayStatus(t *testing.T) {
	out, err := captureStdout(t, func() error {
		printRelayStatus(relayclient.Status{
			Configured: true,
			KeyPrefix:  "wcr_abcdef",
			Connected:  true,
			Generation: "gen-9",
			Revoked:    true,
			LastError:  "boom",
			Served:     42,
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"configured: true\n", "key_prefix: wcr_abcdef\n", "connected: true\n",
		"generation: gen-9\n", "revoked: true\n", "last_error: boom\n", "served: 42\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("out missing %q: %q", want, out)
		}
	}

	// Minimal status omits the optional lines.
	out, _ = captureStdout(t, func() error {
		printRelayStatus(relayclient.Status{})
		return nil
	})
	for _, absent := range []string{"key_prefix", "generation", "last_error"} {
		if strings.Contains(out, absent) {
			t.Fatalf("out should omit %q: %q", absent, out)
		}
	}
}
