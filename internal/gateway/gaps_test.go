package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/tenancy"
)

// fakeSession is a scripted SessionAuthenticator.
type fakeSession struct {
	tc     tenancy.Context
	err    error
	gotTok string
	gotReq *http.Request
	calls  int
}

func (f *fakeSession) AuthenticateRequest(r *http.Request, accessToken string) (tenancy.Context, error) {
	f.calls++
	f.gotTok = accessToken
	f.gotReq = r
	return f.tc, f.err
}

func TestAuthenticateDPoPScheme(t *testing.T) {
	sessionCtx := tenancy.Context{TenantID: "t9", UserID: "u9", Scopes: []string{"runs:read"}}

	t.Run("dpop scheme without session authenticator", func(t *testing.T) {
		h := &okHandler{}
		handler := Authenticate(authn.NewLocal())(h)
		r := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
		r.Header.Set("Authorization", "DPoP wsess_tok")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		if h.reached {
			t.Error("request reached handler despite unsupported scheme")
		}
	})

	t.Run("dpop scheme session rejects token", func(t *testing.T) {
		sess := &fakeSession{err: errors.New("no such session")}
		h := &okHandler{}
		handler := Authenticate(authn.NewLocal(), sess)(h)
		r := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
		r.Header.Set("Authorization", "DPoP wsess_bad")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		if got := w.Header().Get("WWW-Authenticate"); got != "DPoP" {
			t.Errorf("WWW-Authenticate = %q, want DPoP", got)
		}
		if sess.calls != 1 || sess.gotTok != "wsess_bad" {
			t.Errorf("session saw calls=%d token=%q", sess.calls, sess.gotTok)
		}
		if h.reached {
			t.Error("request reached handler despite rejected session")
		}
	})

	t.Run("dpop scheme session accepts and injects context", func(t *testing.T) {
		sess := &fakeSession{tc: sessionCtx}
		h := &okHandler{}
		handler := Authenticate(authn.NewLocal(), sess)(h)
		r := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
		r.Header.Set("Authorization", "dpop wsess_ok") // scheme is case-insensitive
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if !h.reached {
			t.Fatal("request did not reach handler")
		}
		if sess.gotReq == nil || sess.gotReq.URL.Path != "/api/runs" {
			t.Errorf("session request path = %v", sess.gotReq)
		}
		if !h.ok || !reflect.DeepEqual(h.tc, sessionCtx) {
			t.Errorf("tenancy context = %+v ok=%v, want %+v", h.tc, h.ok, sessionCtx)
		}
	})
}

func TestAuthenticateDPoPProofWithoutToken(t *testing.T) {
	sess := &fakeSession{tc: tenancy.Local()}
	h := &okHandler{}
	handler := Authenticate(authn.NewLocal(), sess)(h)

	// A bare DPoP proof header cannot authenticate: there is no access token
	// to bind it to.
	r := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	r.Header.Set("DPoP", "eyJ0eXAiOiJkcG9wK2p3dCIs...")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if h.reached {
		t.Error("request reached handler without an access token")
	}
	if sess.calls != 0 {
		t.Errorf("session consulted %d times, want 0", sess.calls)
	}

	// A DPoP proof alongside a Bearer token is a normal Bearer request: the
	// provider authenticates it, the proof is the handler's business.
	h2 := &okHandler{}
	handler2 := Authenticate(authn.NewStatic("tok", tenancy.Local()), sess)(h2)
	r2 := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	r2.Header.Set("Authorization", "Bearer tok")
	r2.Header.Set("DPoP", "proof")
	w2 := httptest.NewRecorder()
	handler2.ServeHTTP(w2, r2)
	if w2.Code != http.StatusOK || !h2.reached {
		t.Fatalf("bearer+proof: status=%d reached=%v", w2.Code, h2.reached)
	}
}
