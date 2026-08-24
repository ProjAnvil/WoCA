// sessionprovider.go authenticates DPoP-scheme requests against the native
// session store: the Authorization access token resolves the session, the
// DPoP proof header must bind to this request under the key pinned at
// activation (RFC 9449).
package tenant

import (
	"net/http"
	"strings"
	"time"

	"github.com/projanvil/woca/internal/ee/dpop"
	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/platform/tenancy"
)

// SessionProvider authenticates native-client requests. It implements the
// gateway's request-aware session authenticator; the DPoP scheme dispatch
// lives in the gateway middleware.
type SessionProvider struct {
	store    *Store
	verifier *dpop.Verifier
}

// NewSessionProvider returns a session authenticator over st.
func NewSessionProvider(st *Store, verifier *dpop.Verifier) *SessionProvider {
	return &SessionProvider{store: st, verifier: verifier}
}

// AuthenticateRequest resolves the DPoP access token and verifies its proof
// against the request. Any failure is authn.ErrUnauthenticated.
func (p *SessionProvider) AuthenticateRequest(r *http.Request, accessToken string) (tenancy.Context, error) {
	proof := r.Header.Get("DPoP")
	if proof == "" || accessToken == "" {
		return tenancy.Context{}, authn.ErrUnauthenticated
	}
	sess, ok, err := p.store.LookupSession(r.Context(), accessToken)
	if err != nil {
		return tenancy.Context{}, err
	}
	if !ok {
		return tenancy.Context{}, authn.ErrUnauthenticated
	}
	req := dpop.Request{Method: r.Method, URI: RequestURI(r)}
	if err := p.verifier.Verify(proof, req, accessToken, sess.DPoPThumbprint); err != nil {
		return tenancy.Context{}, authn.ErrUnauthenticated
	}
	_ = p.store.TouchSession(r.Context(), accessToken, time.Now().UnixMilli())
	return tenancy.Context{
		TenantID: sess.TenantID,
		UserID:   sess.UserID,
		Scopes:   authz.ScopesForRole(sess.Role),
	}, nil
}

// RequestURI builds the htu the server compares proofs against: the request's
// scheme (TLS-aware), host, and escaped path, without query or fragment. The
// native client constructs the same value from its configured BFF origin.
func RequestURI(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v == "https" || v == "http" {
		scheme = v
	}
	host := strings.ToLower(r.Host)
	return scheme + "://" + host + r.URL.EscapedPath()
}
