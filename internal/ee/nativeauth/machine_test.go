// machine_test.go covers the service-plane machine channel of the native
// surface: activate → introspect (thumbprint for the edge's proof
// checks) → revoke → introspect reports revocation.
package nativeauth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/projanvil/woca/internal/ee/tenant"
)

func TestMachineRoundTrip(t *testing.T) {
	f := newNativeAuthFixture(t)
	h := MachineHandler(New(f.store, tenant.NewAPIKeyProvider(f.store)))
	post := func(path string, body any) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		out := map[string]any{}
		_ = json.NewDecoder(w.Body).Decode(&out)
		return w, out
	}

	w, out := post("/gw/native/activate", map[string]any{
		"api_key": f.key, "device_id": "device-m", "dpop_jwk": json.RawMessage(f.publicJWK()),
	})
	if w.Code != http.StatusOK || out["session_id"] == "" {
		t.Fatalf("machine activate = %d %v", w.Code, out)
	}
	session := out["session_id"].(string)

	w, out = post("/gw/native/introspect", map[string]string{"token": session})
	if w.Code != http.StatusOK || out["active"] != true {
		t.Fatalf("machine introspect = %d %v", w.Code, out)
	}
	if out["dpop_thumbprint"] != f.thumbprint {
		t.Fatalf("thumbprint = %v, want the activating JWK's", out["dpop_thumbprint"])
	}
	if out["role"] == "" || out["tenant_id"] == "" || out["user_id"] == "" {
		t.Fatalf("introspection lacks actor fields: %v", out)
	}

	// A bad key is rejected without an oracle.
	w, out = post("/gw/native/activate", map[string]any{
		"api_key": "woca_wrong", "device_id": "device-m", "dpop_jwk": json.RawMessage(f.publicJWK()),
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad-key machine activate = %d %v", w.Code, out)
	}

	// Revocation is terminal and visible to the introspection.
	w, _ = post("/gw/native/revoke", map[string]string{"token": session})
	if w.Code != http.StatusOK {
		t.Fatalf("machine revoke = %d", w.Code)
	}
	w, out = post("/gw/native/introspect", map[string]string{"token": session})
	if w.Code != http.StatusOK || out["active"] != false || out["code"] != "revoked" {
		t.Fatalf("revoked introspect = %d %v", w.Code, out)
	}
}
