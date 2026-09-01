package main

// The embedded-bridge bootstrap (product design §5.1): install the
// bundled binary on first run, spawn it on a random loopback port, and —
// when no profile exists yet — adopt it as the zero-login default. Also
// builds the credential stack: loopback profiles stay anonymous, remote
// bridges log in with the stored API key, enterprise endpoints fall
// through to DPoP.

import (
	"context"
	"net/url"
	"os"
	"path/filepath"

	"github.com/projanvil/woca/native-client/internal/transport"

	"github.com/projanvil/woca/native-client/internal/bridgeproc"
)

// embeddedBridgeVersion keys the install-on-first-run upgrade check; tie
// it to the client build when the release pipeline bundles the artifact.
const embeddedBridgeVersion = "dev"

// setupEmbeddedBridge installs+starts the bundled bridge and returns it
// with its key store. Best-effort: a failure degrades to manual profiles,
// it never blocks the app. The adoption decision (zero-login local entry
// vs an enrolled bridge's sign-in entry) stays with the caller — it needs
// the enrollment marker and the service, both assembled after the spawn.
func setupEmbeddedBridge(ctx context.Context, dataDir string, spawnArgs []string) (*bridgeproc.Manager, *bridgeproc.KeyStore) {
	data := embeddedBridgeBytes()
	if len(data) == 0 {
		return nil, nil
	}
	src := filepath.Join(os.TempDir(), "woca-bridge-bundled")
	if err := os.WriteFile(src, data, 0o755); err != nil {
		return nil, nil
	}
	bin, err := bridgeproc.Install(src, embeddedBridgeVersion)
	if err != nil {
		return nil, nil
	}
	mgr := bridgeproc.New(bin, spawnArgs...)
	if _, err := mgr.Start(ctx); err != nil {
		return nil, nil
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
