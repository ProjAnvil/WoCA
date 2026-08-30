package main

// The embedded-bridge bootstrap (product design §5.1): install the
// bundled binary on first run, spawn it on a random loopback port, and —
// when no profile exists yet — adopt it as the zero-login default. Also
// builds the credential stack: loopback profiles stay anonymous, remote
// bridges log in with the stored API key, enterprise endpoints fall
// through to DPoP.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"

	"github.com/projanvil/woca/native-client/internal/transport"

	"github.com/projanvil/woca/native-client/internal/bridgeproc"
)

// embeddedBridgeVersion keys the install-on-first-run upgrade check. It
// is the content digest of the bundled binary, so every restaged build
// (a rebuilt woca-bridge with new behavior) replaces the installed copy —
// a constant string silently kept running the previous binary.
func embeddedBridgeVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:6])
}

// setupEmbeddedBridge installs+starts the bundled bridge and adopts it as
// the default profile on first run. Best-effort: a failure degrades to
// manual profiles, it never blocks the app.
func setupEmbeddedBridge(ctx context.Context, dataDir string, firstRun func(baseURL string)) (*bridgeproc.Manager, *bridgeproc.KeyStore) {
	data := embeddedBridgeBytes()
	if len(data) == 0 {
		return nil, nil
	}
	src := filepath.Join(os.TempDir(), "woca-bridge-bundled")
	if err := os.WriteFile(src, data, 0o755); err != nil {
		return nil, nil
	}
	bin, err := bridgeproc.Install(src, embeddedBridgeVersion(data))
	if err != nil {
		return nil, nil
	}
	// Dev ergonomics: a fixed bridge port (WOCA_NATIVE_BRIDGE_ADDR) lets
	// the vite dev server proxy /api at a known target — wails dev's HMR
	// mode loads the SPA from vite, so the embedded server's own asset
	// proxy is not in the loop. The flag is appended after the default
	// --addr, and the later flag wins.
	var extra []string
	if addr := os.Getenv("WOCA_NATIVE_BRIDGE_ADDR"); addr != "" {
		extra = []string{"--addr", addr}
	}
	mgr := bridgeproc.New(bin, extra...)
	baseURL, err := mgr.Start(ctx)
	if err != nil {
		return nil, nil
	}
	if firstRun != nil {
		firstRun(baseURL)
	}
	ks, err := bridgeproc.OpenKeyStore(dataDir)
	if err != nil {
		ks = nil // key logins degrade to per-connection entry
	}
	return mgr, ks
}

// loopbackCredentials serve loopback profiles without any credential
// (trust the machine, product design §5.1 免登录).
type loopbackCredentials struct{}

func (loopbackCredentials) Credentials(context.Context, string, *url.URL) (transport.Credentials, error) {
	return transport.Credentials{}, nil
}
