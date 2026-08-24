package bridgeproc

import (
	"errors"
	"testing"
)

func TestKeyStoreFileFallback(t *testing.T) {
	// No OS keyring in tests: every keyring call fails → file fallback.
	keyringSet = func(string, string, string) error { return errNoKeyring }
	keyringGet = func(string, string) (string, error) { return "", errNoKeyring }
	keyringErase = func(string, string) error { return errNoKeyring }
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	ks, err := OpenKeyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The keyring itself may or may not work headless; both paths must
	// satisfy Get-after-Set.
	if err := ks.Set("https://bridge.example.com", "woca_key_1"); err != nil {
		t.Fatal(err)
	}
	if got := ks.Get("https://bridge.example.com"); got != "woca_key_1" {
		t.Fatalf("get = %q", got)
	}
	if got := ks.Get("https://other.example.com"); got != "" {
		t.Fatalf("absent = %q", got)
	}
	ks.Delete("https://bridge.example.com")
	if got := ks.Get("https://bridge.example.com"); got != "" {
		t.Fatalf("after delete = %q", got)
	}
}

var errNoKeyring = errors.New("no keyring in tests")
