// credentials_dpop.go mints per-request DPoP proofs (RFC 9449) from the
// device key and the stored opaque session. The proof binds the HTTP method
// and the BFF destination URI (without query), so a captured proof cannot be
// replayed against another endpoint.
package transport

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/projanvil/woca/internal/platform/dpop"
	"github.com/projanvil/woca/native-client/internal/devicekey"
)

// ErrNoNativeSession reports that no usable session is stored: the caller
// surfaces the enterprise connection gate.
var ErrNoNativeSession = errors.New("native session is not available")

// SessionSource resolves the current opaque session id.
type SessionSource func() (string, bool)

// DPoPCredentials implements CredentialProvider with per-request proofs.
type DPoPCredentials struct {
	material devicekey.Material
	session  SessionSource
	now      func() time.Time
}

// NewDPoPCredentials returns a DPoP credential provider.
func NewDPoPCredentials(material devicekey.Material, session SessionSource) *DPoPCredentials {
	return &DPoPCredentials{material: material, session: session, now: time.Now}
}

// Credentials builds the DPoP Authorization header pair for one request.
func (p *DPoPCredentials) Credentials(_ context.Context, method string, destination *url.URL) (Credentials, error) {
	session, ok := p.session()
	if !ok || session == "" {
		return Credentials{}, ErrNoNativeSession
	}
	htu := destinationHTU(destination)
	jti, err := randomJTI()
	if err != nil {
		return Credentials{}, err
	}
	proof, err := dpop.Sign(p.material.DPoPKey, dpop.ProofInput{
		HTM:         method,
		HTU:         htu,
		AccessToken: session,
		IAT:         p.now(),
		JTI:         jti,
	})
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{Scheme: "DPoP", Session: session, Proof: proof}, nil
}

// destinationHTU renders the normalized htu of a destination: lowercase
// scheme and host, escaped path, no query or fragment — matching the
// server's RequestURI comparison.
func destinationHTU(dest *url.URL) string {
	scheme := strings.ToLower(dest.Scheme)
	host := strings.ToLower(dest.Host)
	return scheme + "://" + host + dest.EscapedPath()
}

func randomJTI() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
