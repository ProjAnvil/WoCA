package relayclient

import (
	"os"
	"strings"

	"github.com/projanvil/woca/internal/settings"
)

// Environment equivalents of `woca relay connect` (design §3.2 —
// container first-boot / headless deployments). Environment wins over
// persisted settings.
const (
	EnvRelayURL = "WOCA_RELAY_URL"
	EnvRelayKey = "WOCA_RELAY_KEY"
)

// ResolveConfig reads the relay configuration: environment first, then
// the settings store (URL is a plain settings row; the key rides the
// encrypted secrets table — never plaintext on disk, design §7.1).
func ResolveConfig(st *settings.Store) (url, key string) {
	url = os.Getenv(EnvRelayURL)
	key = os.Getenv(EnvRelayKey)
	if url != "" && key != "" {
		return url, key
	}
	if st == nil {
		return url, key
	}
	if url == "" {
		url = st.Load().RelayURL
	}
	if key == "" {
		key = st.GetSecret(settings.SecretRelayKey)
	}
	// Canonical form: no trailing slashes (endpoints join as
	// URL+"/tunnel/poll"; a double slash breaks POST respond through
	// ServeMux redirects).
	return strings.TrimRight(url, "/"), key
}

// SaveConfig persists the relay configuration (`woca relay connect`).
// The URL is stored canonicalized (trailing slashes trimmed).
func SaveConfig(st *settings.Store, url, key string) error {
	url = strings.TrimRight(url, "/")
	cfg := st.Load()
	if err := settings.Apply(&cfg, settings.KeyRelayURL, url); err != nil {
		return err
	}
	if err := st.Save(cfg); err != nil {
		return err
	}
	return st.SetSecret(settings.SecretRelayKey, key)
}

// ClearConfig removes the persisted relay configuration (`woca relay
// disconnect`). Save skips empty fields by design, so the URL row goes
// through Store.Clear and the secret through SetSecret("").
func ClearConfig(st *settings.Store) error {
	if err := st.Clear("relay_url"); err != nil {
		return err
	}
	return st.SetSecret(settings.SecretRelayKey, "")
}
