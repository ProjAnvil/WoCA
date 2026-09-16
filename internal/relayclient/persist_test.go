package relayclient

import (
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/settings"
)

func openTestSettings(t *testing.T) *settings.Store {
	t.Helper()
	st, err := settings.Open(filepath.Join(t.TempDir(), "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("settings store disabled")
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestSaveResolveClear(t *testing.T) {
	st := openTestSettings(t)
	if err := SaveConfig(st, "https://relay.example.com", "wcr_secret"); err != nil {
		t.Fatal(err)
	}
	url, key := ResolveConfig(st)
	if url != "https://relay.example.com" || key != "wcr_secret" {
		t.Fatalf("resolved %q / %q", url, key)
	}
	// The URL is a plain settings row; the key is encrypted-only
	// (settings secrets, design §7.1 — never plaintext on disk).
	if got := settings.KeyValue(st.Load(), settings.KeyRelayURL); got != "https://relay.example.com" {
		t.Fatalf("relay url row: %q", got)
	}
	if !st.HasSecret(settings.SecretRelayKey) {
		t.Fatal("relay key missing from the secrets table")
	}
	if err := ClearConfig(st); err != nil {
		t.Fatal(err)
	}
	url, key = ResolveConfig(st)
	if url != "" || key != "" {
		t.Fatalf("after clear: %q / %q", url, key)
	}
	if st.HasSecret(settings.SecretRelayKey) {
		t.Fatal("clear left the relay key behind")
	}
}

func TestEnvOverridesSettings(t *testing.T) {
	st := openTestSettings(t)
	if err := SaveConfig(st, "https://stored.example.com", "wcr_stored"); err != nil {
		t.Fatal(err)
	}
	// Container first-boot / headless deployments use the env pair
	// (design §3.2); env wins over persisted values.
	t.Setenv(EnvRelayURL, "https://env.example.com")
	t.Setenv(EnvRelayKey, "wcr_env")
	url, key := ResolveConfig(st)
	if url != "https://env.example.com" || key != "wcr_env" {
		t.Fatalf("env precedence broken: %q / %q", url, key)
	}
}

func TestResolveNilStore(t *testing.T) {
	// A nil store (settings disabled) still honors the environment.
	t.Setenv(EnvRelayURL, "https://env.example.com")
	url, key := ResolveConfig(nil)
	if url != "https://env.example.com" || key != "" {
		t.Fatalf("nil store: %q / %q", url, key)
	}
}

func TestTrailingSlashCanonicalized(t *testing.T) {
	st := openTestSettings(t)
	// A trailing slash must never persist: endpoints join as
	// URL+"/tunnel/poll" and "//tunnel/poll" breaks POST respond through
	// ServeMux redirects.
	if err := SaveConfig(st, "https://relay.example.com/", "wcr_secret"); err != nil {
		t.Fatal(err)
	}
	url, _ := ResolveConfig(st)
	if url != "https://relay.example.com" {
		t.Fatalf("stored url %q, want canonical no-trailing-slash form", url)
	}
	// The env path is canonicalized too.
	t.Setenv(EnvRelayURL, "https://env.example.com//")
	url, _ = ResolveConfig(st)
	if url != "https://env.example.com" {
		t.Fatalf("env url %q, want canonical form", url)
	}
}
