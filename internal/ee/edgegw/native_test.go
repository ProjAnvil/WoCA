// native_test.go covers the edge's native-client surface end to end over
// the fake core: activation proxies to the machine channel, DPoP proofs
// verify at the edge origin, the assertion rides the tunnel like a
// browser session, and the bridge inventory only lists servable rows.
package edgegw

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/ee/dpop"
	"github.com/projanvil/woca/internal/platform/machineid"
)

// nativeDevice is one activating native client: its API key, device key
// pair, and the session the activation returns.
type nativeDevice struct {
	t        *testing.T
	key      string
	dpopKey  *ecdsa.PrivateKey
	session  string
	edgeURL  string
	deviceID string
}

func newNativeDevice(t *testing.T, core *fakeCore, edgeURL string) *nativeDevice {
	t.Helper()
	u, err := core.keys.CreateUser(t.Context(), "local", "native-user", "tenant-admin")
	if err != nil {
		t.Fatal(err)
	}
	_, full, err := core.keys.IssueKey(t.Context(), "local", u.ID, "native-user", "tenant-admin")
	if err != nil {
		t.Fatal(err)
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &nativeDevice{t: t, key: full, dpopKey: priv, edgeURL: edgeURL, deviceID: "device-native"}
}

func (d *nativeDevice) publicJWK() []byte {
	d.t.Helper()
	return []byte(fmt.Sprintf(`{"kty":"EC","crv":"P-256","x":"%s","y":"%s"}`,
		base64.RawURLEncoding.EncodeToString(d.dpopKey.PublicKey.X.Bytes()),
		base64.RawURLEncoding.EncodeToString(d.dpopKey.PublicKey.Y.Bytes())))
}

// activate exchanges the key at the edge and remembers the session.
func (d *nativeDevice) activate() {
	d.t.Helper()
	body, _ := json.Marshal(map[string]any{
		"api_key": d.key, "device_id": d.deviceID, "dpop_jwk": json.RawMessage(d.publicJWK()),
	})
	resp, err := http.Post(d.edgeURL+"/api/native/activate", "application/json", strings.NewReader(string(body)))
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		SessionID string `json:"session_id"`
		ExpiresAt int64  `json:"expires_at"`
		DeviceID  string `json:"device_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.SessionID == "" || out.ExpiresAt == 0 || out.DeviceID != d.deviceID {
		d.t.Fatalf("edge activate = %d %+v", resp.StatusCode, out)
	}
	d.session = out.SessionID
}

// sign builds the DPoP proof for one request at the edge origin.
func (d *nativeDevice) sign(method, uri, token string) string {
	d.t.Helper()
	proof, err := dpop.Sign(d.dpopKey, dpop.ProofInput{
		HTM: method, HTU: uri, AccessToken: token,
		IAT: time.Now(), JTI: fmt.Sprintf("jti-%d", time.Now().UnixNano()),
	})
	if err != nil {
		d.t.Fatal(err)
	}
	return proof
}

// request performs one DPoP-authenticated call through the edge.
func (d *nativeDevice) request(method, path string, proofOK bool) *http.Response {
	d.t.Helper()
	uri := d.edgeURL + path
	req, _ := http.NewRequest(method, uri, nil)
	req.Header.Set("Authorization", "DPoP "+d.session)
	if proofOK {
		req.Header.Set("DPoP", d.sign(method, uri, d.session))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	return resp
}

// enrollNamed registers one bridge and returns (access token, bridge id).
func enrollNamed(t *testing.T, core *fakeCore) (string, string) {
	t.Helper()
	token, err := core.store.IssueEnrollment(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	bridgeID, err := core.store.ConsumeEnrollment(context.Background(), machineid.HashToken(token), []byte("native-test-pub"), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	access, _, err := core.store.IssueAccess(context.Background(), bridgeID, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return access, bridgeID
}

// TestNativeActivateThroughEdge: the public activation proxies to the
// core's machine channel and hands back the device-bound session; a bad
// key is a uniform 401 (never an oracle).
func TestNativeActivateThroughEdge(t *testing.T) {
	core := newFakeCore(t)
	edge := newEdge(t, core)
	newNativeDevice(t, core, edge.URL).activate()

	body := `{"api_key":"woca_wrong","device_id":"d","dpop_jwk":{"kty":"EC","crv":"P-256","x":"AAA","y":"AAA"}}`
	resp, err := http.Post(edge.URL+"/api/native/activate", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad-key activate = %d, want 401", resp.StatusCode)
	}
	var coded struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&coded)
	if coded.Code != "S-E-0001" {
		t.Fatalf("bad-key code = %q, want S-E-0001", coded.Code)
	}
}

// TestNativeSessionLifecycle: session reads and revocation verify the
// DPoP proof at the edge origin; a missing proof or a wrong-origin proof
// fails closed; after revocation the token is terminal.
func TestNativeSessionLifecycle(t *testing.T) {
	core := newFakeCore(t)
	edge := newEdge(t, core)
	dev := newNativeDevice(t, core, edge.URL)
	dev.activate()

	resp := dev.request(http.MethodGet, "/api/native/session", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session read = %d, want 200", resp.StatusCode)
	}
	var info struct {
		ExpiresAt       int64  `json:"expires_at"`
		DeviceID        string `json:"device_id"`
		LastValidatedAt int64  `json:"last_validated_at"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if info.DeviceID != dev.deviceID || info.ExpiresAt == 0 {
		t.Fatalf("session info = %+v", info)
	}

	// No proof: fail closed.
	resp = dev.request(http.MethodGet, "/api/native/session", false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("proofless session read = %d, want 401", resp.StatusCode)
	}

	// Proof signed for a different origin: fail closed.
	other := dev.sign(http.MethodGet, "https://elsewhere.example.com/api/native/session", dev.session)
	req, _ := http.NewRequest(http.MethodGet, edge.URL+"/api/native/session", nil)
	req.Header.Set("Authorization", "DPoP "+dev.session)
	req.Header.Set("DPoP", other)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-origin proof = %d, want 401", resp.StatusCode)
	}

	// Logout revokes; the next read is terminal (0002).
	resp = dev.request(http.MethodDelete, "/api/native/session", true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("native logout = %d, want 204", resp.StatusCode)
	}
	resp = dev.request(http.MethodGet, "/api/native/session", true)
	defer resp.Body.Close()
	var coded struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&coded)
	if resp.StatusCode != http.StatusUnauthorized || coded.Code != "S-E-0002" {
		t.Fatalf("revoked session = %d %q, want 401 S-E-0002", resp.StatusCode, coded.Code)
	}
}

// TestNativeAPITrafficTunneled: a DPoP-authenticated API call routes to
// the pinned bridge and lands with the native session's assertion — the
// credential and proof never reach the bridge.
func TestNativeAPITrafficTunneled(t *testing.T) {
	parkPickupTimeout = 5 * time.Second
	defer func() { parkPickupTimeout = 30 * time.Second }()
	core := newFakeCore(t)
	edge := newEdge(t, core)
	access, bridgeID := enrollNamed(t, core)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runTunnel(ctx, t, core, edge.URL, access, echoBridge(bridgeID))

	dev := newNativeDevice(t, core, edge.URL)
	dev.activate()

	req, _ := http.NewRequest(http.MethodGet, edge.URL+"/api/who", nil)
	req.Header.Set("Authorization", "DPoP "+dev.session)
	req.Header.Set("DPoP", dev.sign(http.MethodGet, edge.URL+"/api/who", dev.session))
	req.Header.Set("X-Woca-Bridge", bridgeID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var who map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&who)
	if resp.StatusCode != http.StatusOK || who["bridge"] != bridgeID {
		t.Fatalf("native tunnel = %d %v", resp.StatusCode, who)
	}
	if who["actor"] == "" {
		t.Fatalf("native traffic reached the bridge without an actor assertion: %v", who)
	}
	if who["bearer"] != "" {
		t.Fatalf("credential leaked through the tunnel: %q", who["bearer"])
	}
}

// TestBridgeListFiltersUnservable: the pre-auth inventory only lists
// bridges a client can actually reach — live with an open tunnel. A
// local-trust (tunnel closed) bridge and stale offline registrations are
// noise that dead-ends every picker.
func TestBridgeListFiltersUnservable(t *testing.T) {
	core := newFakeCore(t)
	edge := newEdge(t, core)

	beat := func(t *testing.T, access, status string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, core.svc.URL+"/gw/heartbeat", strings.NewReader(status))
		req.Header.Set("Authorization", "Bearer "+access)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	localAccess, _ := enrollNamed(t, core)  // native-style: live, tunnel closed
	remoteAccess, _ := enrollNamed(t, core) // container-style: live, tunnel open
	_, _ = enrollNamed(t, core)             // stale registration: never beats
	beat(t, localAccess, `{"tunnel":false}`)
	beat(t, remoteAccess, `{"tunnel":true}`)

	resp, err := http.Get(edge.URL + "/api/edge/bridges")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var bridges []struct {
		BridgeID string `json:"bridge_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&bridges)
	if len(bridges) != 1 {
		t.Fatalf("inventory = %+v, want only the servable bridge", bridges)
	}
}
