package entrygw

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/platform/machineid"
	"github.com/projanvil/woca/internal/wocadb"
)

func newGW(t *testing.T) (*Store, *httptest.Server) {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "gw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	// Both planes on one mux — the same union the edge presents to bridges.
	mux := http.NewServeMux()
	NewRegistryHandler(store).RegisterRoutes(mux)
	NewConfigHandler(store, func(_ context.Context, bridgeID string) (string, map[string]string, error) {
		return "gen-7", map[string]string{
			"provider":        "openai",
			"model":           "gpt-test",
			"model-whitelist": "gpt-test,gpt-mini",
		}, nil
	}).RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return store, ts
}

// enrollBridge consumes a fresh enrollment and returns (plaintext access
// token, bridge id).
func enrollBridge(t *testing.T, store *Store, ts *httptest.Server) (string, string) {
	t.Helper()
	token, err := store.IssueEnrollment(context.Background(), EnrollmentTTL)
	if err != nil {
		t.Fatal(err)
	}
	priv, _ := machineid.GenerateIdentity()
	pub, _ := machineid.PublicKeyPEM(priv.Public().(ed25519.PublicKey))
	body, _ := json.Marshal(map[string]any{"enrollment_token": token, "public_key": pub})
	resp, err := http.Post(ts.URL+"/gw/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enroll = %d", resp.StatusCode)
	}
	var out struct {
		BridgeID    string `json:"bridge_id"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	// The enrollment token is spent with the enrollment.
	return out.AccessToken, out.BridgeID
}

func get(t *testing.T, url, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestEnrollPullHeartbeatRevoke walks the full channel acceptance shape.
func TestEnrollPullHeartbeatRevoke(t *testing.T) {
	store, ts := newGW(t)
	access, bridgeID := enrollBridge(t, store, ts)

	// Pull config with the access token.
	resp, err := httpGetAuth(ts.URL+"/gw/config", access)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("config = %d", resp.StatusCode)
	}
	var cfg struct {
		Version  string            `json:"version"`
		Settings map[string]string `json:"settings"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&cfg)
	if cfg.Version != "gen-7" || cfg.Settings["model-whitelist"] == "" {
		t.Fatalf("config = %+v", cfg)
	}

	// Unchanged version → 304 (never a cache-clearing signal, §3.2).
	if code := get(t, ts.URL+"/gw/config?version=gen-7", access); code != http.StatusNotModified {
		t.Fatalf("unchanged version = %d, want 304", code)
	}

	// Heartbeat records into the service plane.
	hb, _ := json.Marshal(map[string]any{"active_runs": 1})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/gw/heartbeat", bytes.NewReader(hb))
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("heartbeat = %d", resp2.StatusCode)
	}

	// Anonymous requests are rejected.
	if code := get(t, ts.URL+"/gw/config", ""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous config = %d", code)
	}

	// Renewal rotates the token: the old one stops working.
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/gw/renew", nil)
	req2.Header.Set("Authorization", "Bearer "+access)
	resp3, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	var renewed struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp3.Body).Decode(&renewed)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK || renewed.AccessToken == "" || renewed.AccessToken == access {
		t.Fatalf("renew = %d %q", resp3.StatusCode, renewed.AccessToken)
	}
	if code := get(t, ts.URL+"/gw/config", access); code != http.StatusUnauthorized {
		t.Fatalf("old token after renewal = %d, want 401", code)
	}

	// Revocation cascades: the renewed token dies immediately.
	if err := store.RevokeBridge(context.Background(), bridgeID); err != nil {
		t.Fatal(err)
	}
	if code := get(t, ts.URL+"/gw/config", renewed.AccessToken); code != http.StatusUnauthorized {
		t.Fatalf("revoked bridge config = %d, want 401", code)
	}
}

// TestExpiredEnrollmentRejected: stale tokens cannot enroll.
func TestExpiredEnrollmentRejected(t *testing.T) {
	store, ts := newGW(t)
	token, err := store.IssueEnrollment(context.Background(), -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	priv, _ := machineid.GenerateIdentity()
	pub, _ := machineid.PublicKeyPEM(priv.Public().(ed25519.PublicKey))
	body, _ := json.Marshal(map[string]any{"enrollment_token": token, "public_key": pub})
	resp, _ := http.Post(ts.URL+"/gw/enroll", "application/json", bytes.NewReader(body))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired enrollment = %d, want 401", resp.StatusCode)
	}
}

func httpGetAuth(url, token string) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return http.DefaultClient.Do(req)
}

// TestBlockingConfigQuery: a parked long poll returns the moment the
// version moves (push semantics over the pull channel, §2.3).
func TestBlockingConfigQuery(t *testing.T) {
	var mu sync.Mutex
	version := "v1"
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "gw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewRegistryHandler(store).RegisterRoutes(mux)
	NewConfigHandler(store, func(context.Context, string) (string, map[string]string, error) {
		mu.Lock()
		defer mu.Unlock()
		return version, map[string]string{"provider": "openai"}, nil
	}).RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	access, _ := enrollBridge(t, store, ts)
	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/gw/config?version=v1&wait=20s", nil)
		req.Header.Set("Authorization", "Bearer "+access)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- -1
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	version = "v2" // the "push"
	mu.Unlock()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("blocking query = %d, want 200 (new version)", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocking query did not return after the version moved")
	}
}

// TestWorkerLedEnrollment: request → pending list → approve → the bridge's
// status poll delivers the token (zero pre-shared secrets).
func TestWorkerLedEnrollment(t *testing.T) {
	store, ts := newGW(t)

	priv, _ := machineid.GenerateIdentity()
	pub, _ := machineid.PublicKeyPEM(priv.Public().(ed25519.PublicKey))
	body, _ := json.Marshal(map[string]any{"public_key": pub})
	resp, err := http.Post(ts.URL+"/gw/enroll-request", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Fingerprint string `json:"fingerprint"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || out.Fingerprint == "" {
		t.Fatalf("request = %d %+v", resp.StatusCode, out)
	}

	// Not yet approved: status poll says pending.
	poll, _ := http.Get(ts.URL + "/gw/enroll-status?fingerprint=" + out.Fingerprint)
	poll.Body.Close()
	if poll.StatusCode != http.StatusAccepted {
		t.Fatalf("unapproved status = %d", poll.StatusCode)
	}

	pending, err := store.PendingRequests(context.Background())
	if err != nil || len(pending) != 1 || pending[0].Fingerprint != out.Fingerprint {
		t.Fatalf("pending = %v %v", pending, err)
	}
	if _, err := store.ApproveEnrollment(context.Background(), out.Fingerprint); err != nil {
		t.Fatal(err)
	}

	poll2, err := http.Get(ts.URL + "/gw/enroll-status?fingerprint=" + out.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer poll2.Body.Close()
	if poll2.StatusCode != http.StatusOK {
		t.Fatalf("approved status = %d", poll2.StatusCode)
	}
	var approved struct {
		EnrollmentToken string `json:"enrollment_token"`
	}
	_ = json.NewDecoder(poll2.Body).Decode(&approved)
	if !machineid.ValidEnrollment(approved.EnrollmentToken) {
		t.Fatalf("token = %q", approved.EnrollmentToken)
	}

	// The delivered token completes the normal enrollment path.
	body2, _ := json.Marshal(map[string]any{"enrollment_token": approved.EnrollmentToken, "public_key": pub})
	enrollResp, _ := http.Post(ts.URL+"/gw/enroll", "application/json", bytes.NewReader(body2))
	enrollResp.Body.Close()
	if enrollResp.StatusCode != http.StatusOK {
		t.Fatalf("enroll with delivered token = %d", enrollResp.StatusCode)
	}
}

// TestGatewaySettingsAndHello: the allow_insecure policy persists and the
// anonymous hello probe reports the posture to bridges/operators.
func TestGatewaySettingsAndHello(t *testing.T) {
	store, ts := newGW(t)

	resp, err := http.Get(ts.URL + "/gw/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var hello struct {
		Service       string `json:"service"`
		AllowInsecure bool   `json:"allow_insecure"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&hello)
	if hello.Service != "woca-entrygw" || hello.AllowInsecure {
		t.Fatalf("hello = %+v", hello)
	}

	if err := store.SetSetting(context.Background(), SettingKeyAllowInsecure, "true"); err != nil {
		t.Fatal(err)
	}
	if !store.AllowInsecure(context.Background()) {
		t.Fatal("setting not persisted")
	}
	resp2, err := http.Get(ts.URL + "/gw/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	_ = json.NewDecoder(resp2.Body).Decode(&hello)
	if !hello.AllowInsecure {
		t.Fatal("hello does not reflect the development posture")
	}
}

// TestResolveAndBridges: the core-internal routes the edge calls home on
// (R7) — token-hash resolution and the live bridge inventory.
func TestResolveAndBridges(t *testing.T) {
	store, ts := newGW(t)
	access, bridgeID := enrollBridge(t, store, ts)

	// Resolve maps the hashed token to the bridge id; hashes only.
	hash := machineid.HashToken(access)
	body, _ := json.Marshal(map[string]string{"token_hash": hash})
	resp, err := http.Post(ts.URL+"/gw/resolve", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		BridgeID string `json:"bridge_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || out.BridgeID != bridgeID {
		t.Fatalf("resolve = %d %+v (want %s)", resp.StatusCode, out, bridgeID)
	}

	// Unknown and malformed hashes never leak which part failed.
	bad, _ := json.Marshal(map[string]string{"token_hash": machineid.HashToken("woca_not_a_token")})
	resp2, _ := http.Post(ts.URL+"/gw/resolve", "application/json", bytes.NewReader(bad))
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown resolve = %d", resp2.StatusCode)
	}
	resp3, err := http.Post(ts.URL+"/gw/resolve", "application/json", bytes.NewReader([]byte(`{"token_hash":"short"}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed resolve = %d", resp3.StatusCode)
	}

	// The bridge inventory lists the enrolled bridge; live only after a beat.
	list, err := http.Get(ts.URL + "/gw/bridges")
	if err != nil {
		t.Fatal(err)
	}
	var bridges []BridgeInfo
	_ = json.NewDecoder(list.Body).Decode(&bridges)
	list.Body.Close()
	if len(bridges) != 1 || bridges[0].BridgeID != bridgeID || bridges[0].Live {
		t.Fatalf("bridges = %+v", bridges)
	}
	hb, _ := json.Marshal(map[string]any{})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/gw/heartbeat", bytes.NewReader(hb))
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	beat, _ := http.DefaultClient.Do(req)
	beat.Body.Close()
	list2, _ := http.Get(ts.URL + "/gw/bridges")
	var bridges2 []BridgeInfo
	_ = json.NewDecoder(list2.Body).Decode(&bridges2)
	list2.Body.Close()
	if len(bridges2) != 1 || !bridges2[0].Live {
		t.Fatalf("bridges after beat = %+v", bridges2)
	}

	// Revocation removes the bridge from the inventory and kills the token.
	if err := store.RevokeBridge(context.Background(), bridgeID); err != nil {
		t.Fatal(err)
	}
	list3, _ := http.Get(ts.URL + "/gw/bridges")
	var bridges3 []BridgeInfo
	_ = json.NewDecoder(list3.Body).Decode(&bridges3)
	list3.Body.Close()
	if len(bridges3) != 0 {
		t.Fatalf("bridges after revoke = %+v", bridges3)
	}
}
