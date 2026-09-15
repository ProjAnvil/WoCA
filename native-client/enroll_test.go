package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/woca/native-client/internal/bridge"
)

// fakeProc is a scriptable bridgeProcess: it records posture restarts and
// answers BaseURL from its current script.
type fakeProc struct {
	mu       sync.Mutex
	base     string
	restarts [][]string
	// failOnArgs makes the restart with exactly these args fail once.
	failOnArgs []string
}

func (f *fakeProc) BaseURL() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.base == "" {
		return "", os.ErrNotExist
	}
	return f.base, nil
}

func (f *fakeProc) RestartWith(_ context.Context, extraArgs ...string) (string, error) {
	f.mu.Lock()
	f.restarts = append(f.restarts, append([]string(nil), extraArgs...))
	fail := len(f.failOnArgs) > 0 && argsEqual(f.failOnArgs, extraArgs)
	f.mu.Unlock()
	if fail {
		return "", context.DeadlineExceeded
	}
	// The port is randomized per spawn in production; the fake keeps one
	// URL — the flip does not change what the tests assert.
	return f.base, nil
}

func argsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (f *fakeProc) restartsWith() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.restarts))
	copy(out, f.restarts)
	return out
}

// bridgeStub fakes the embedded bridge's owner surface (cloud-access).
type bridgeStub struct {
	mu       sync.Mutex
	putBody  map[string]string
	enrolled bool
	failPut  bool
}

func (s *bridgeStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/cloud-access", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		id := ""
		if s.enrolled {
			id = "br_1"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"enrolled": s.enrolled, "bridge_id": id})
	})
	mux.HandleFunc("PUT /api/cloud-access", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.failPut {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		s.putBody = req
		// Configure restarts the gateway client; enrollment completes on
		// the first poll in these tests.
		s.enrolled = true
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"enrolled": true, "bridge_id": "br_1"})
	})
	mux.HandleFunc("POST /api/cloud-access/reset", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.enrolled = false
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"enrolled": false})
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"auth":  "local",
			"cloud": map[string]any{"enrolled": s.enrolled, "url": "https://edge.example.com", "bridge_id": "br_1"},
		})
	})
	return mux
}

// edgeStub fakes the edge's pre-auth inventory.
type edgeStub struct {
	mu     sync.Mutex
	live   bool
	exists bool
}

func (s *edgeStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/edge/bridges", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var out []EdgeBridge
		if s.exists {
			out = append(out, EdgeBridge{BridgeID: "br_1", Live: s.live})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	return mux
}

type enrollFixture struct {
	proc    *fakeProc
	stub    *bridgeStub
	edge    *edgeStub
	bridge  *httptest.Server
	edgeSrv *httptest.Server
	enr     *enroller
	paths   []string // published base URLs
}

func newEnrollFixture(t *testing.T, connect enrollConnect) *enrollFixture {
	t.Helper()
	f := &enrollFixture{
		proc: &fakeProc{},
		stub: &bridgeStub{},
		edge: &edgeStub{exists: true, live: true},
	}
	f.bridge = httptest.NewServer(f.stub.handler())
	f.edgeSrv = httptest.NewServer(f.edge.handler())
	f.proc.base = f.bridge.URL
	f.enr = newEnroller(f.proc, connect, func(b string) { f.paths = append(f.paths, b) }, filepath.Join(t.TempDir(), "marker.json"))
	f.enr.enrollWait = 2 * time.Second
	f.enr.liveWait = 2 * time.Second
	f.enr.pollStep = 20 * time.Millisecond
	t.Cleanup(f.bridge.Close)
	t.Cleanup(f.edgeSrv.Close)
	return f
}

func validInput(edgeURL string) EnrollEmbeddedInput {
	return EnrollEmbeddedInput{
		EdgeURL:         edgeURL,
		EnrollmentToken: "woca_enroll_" + strings.Repeat("a", 16),
		APIKey:          "woca_key1",
	}
}

func TestEnrollHappyPath(t *testing.T) {
	var gotInput bridge.ConnectionInput
	f := newEnrollFixture(t, func(_ context.Context, in bridge.ConnectionInput) (bridge.BootstrapState, error) {
		gotInput = in
		return bridge.BootstrapState{State: bridge.StateReady, ServerURL: in.ServerURL}, nil
	})
	res := f.enr.enroll(t.Context(), validInput(f.edgeSrv.URL))
	if res.Error != "" {
		t.Fatalf("error class = %q", res.Error)
	}
	if res.State.State != bridge.StateReady {
		t.Fatalf("state = %v", res.State.State)
	}
	if res.BridgeID != "br_1" {
		t.Fatalf("bridge id = %q", res.BridgeID)
	}
	// The login rode the edge with the enrolled bridge pinned.
	if gotInput.ServerURL != f.edgeSrv.URL || gotInput.BridgeID != "br_1" {
		t.Fatalf("connect input = %+v", gotInput)
	}
	// The posture flipped to apikey exactly once, and the URL was re-published.
	restarts := f.proc.restartsWith()
	if len(restarts) != 1 || !argsEqual(restarts[0], []string{"--auth", "apikey"}) {
		t.Fatalf("restarts = %v", restarts)
	}
	if len(f.paths) != 1 || f.paths[0] != f.proc.base {
		t.Fatalf("published = %v", f.paths)
	}
	// The marker records the enrollment.
	m := loadEnrollMarker(f.enr.path)
	if !m.Enrolled || m.BridgeID != "br_1" || m.EdgeURL != f.edgeSrv.URL {
		t.Fatalf("marker = %+v", m)
	}
}

func TestEnrollEdgeUnreachable(t *testing.T) {
	f := newEnrollFixture(t, nil)
	edgeURL := httptest.NewServer(http.NotFoundHandler())
	defer edgeURL.Close()
	res := f.enr.enroll(t.Context(), validInput(edgeURL.URL))
	if res.Error != enrollErrEdge {
		t.Fatalf("class = %q, want %q", res.Error, enrollErrEdge)
	}
	// The token was never written — the precheck runs first.
	if f.stub.putBody != nil {
		t.Fatalf("cloud-access write happened: %+v", f.stub.putBody)
	}
}

func TestEnrollTokenPrefixRejected(t *testing.T) {
	f := newEnrollFixture(t, nil)
	in := validInput(f.edgeSrv.URL)
	in.EnrollmentToken = "woca_userkey_wrongshape"
	if res := f.enr.enroll(t.Context(), in); res.Error != enrollErrToken {
		t.Fatalf("class = %q, want %q", res.Error, enrollErrToken)
	}
}

func TestEnrollTokenNeverConsumed(t *testing.T) {
	f := newEnrollFixture(t, nil)
	f.stub.failPut = false
	f.stub.mu.Lock()
	f.stub.enrolled = false
	// PUT succeeds but enrollment never completes → poll timeout.
	f.stub.putBody = nil
	origHandler := f.stub.handler()
	_ = origHandler
	f.stub.mu.Unlock()
	// Swap the PUT handler for one that records but never enrolls.
	f.bridge.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"enrolled": false})
			return
		}
		f.stub.handler().ServeHTTP(w, r)
	})
	res := f.enr.enroll(t.Context(), validInput(f.edgeSrv.URL))
	if res.Error != enrollErrToken {
		t.Fatalf("class = %q, want %q", res.Error, enrollErrToken)
	}
	if _, err := os.Stat(f.enr.path); !os.IsNotExist(err) {
		t.Fatal("marker written despite failed enrollment")
	}
}

func TestEnrollUserKeyRejectedStillEnrolled(t *testing.T) {
	f := newEnrollFixture(t, func(_ context.Context, _ bridge.ConnectionInput) (bridge.BootstrapState, error) {
		return bridge.BootstrapState{State: bridge.StateAuthorizationNeeded}, context.DeadlineExceeded
	})
	res := f.enr.enroll(t.Context(), validInput(f.edgeSrv.URL))
	if res.Error != enrollErrKey {
		t.Fatalf("class = %q, want %q", res.Error, enrollErrKey)
	}
	// The machine stays enrolled — a user-plane failure must not roll it back.
	if m := loadEnrollMarker(f.enr.path); !m.Enrolled {
		t.Fatalf("marker = %+v", m)
	}
	if res.BridgeID != "br_1" {
		t.Fatalf("bridge id = %q", res.BridgeID)
	}
}

func TestEnrollFlipFailureRollsBackToLocal(t *testing.T) {
	f := newEnrollFixture(t, nil)
	f.proc.failOnArgs = []string{"--auth", "apikey"}
	res := f.enr.enroll(t.Context(), validInput(f.edgeSrv.URL))
	if res.Error != enrollErrFlip {
		t.Fatalf("class = %q, want %q", res.Error, enrollErrFlip)
	}
	// The failed flip rolled the process back to the local posture.
	restarts := f.proc.restartsWith()
	if len(restarts) != 2 || !argsEqual(restarts[1], nil) {
		t.Fatalf("restarts = %v, want apikey attempt + local rollback", restarts)
	}
	if _, err := os.Stat(f.enr.path); !os.IsNotExist(err) {
		t.Fatal("marker written despite failed flip")
	}
}

func TestEnrollBridgeLiveTimeout(t *testing.T) {
	f := newEnrollFixture(t, nil)
	f.edge.exists = false
	if res := f.enr.enroll(t.Context(), validInput(f.edgeSrv.URL)); res.Error != enrollErrFlip {
		t.Fatalf("class = %q, want %q", res.Error, enrollErrFlip)
	}
}

func TestDeregisterEmbedded(t *testing.T) {
	f := newEnrollFixture(t, nil)
	if err := saveEnrollMarker(f.enr.path, enrollMarker{Enrolled: true, EdgeURL: f.edgeSrv.URL, BridgeID: "br_1"}); err != nil {
		t.Fatal(err)
	}
	res := f.enr.deregister(t.Context())
	if res.Error != "" {
		t.Fatalf("error class = %q", res.Error)
	}
	if res.State.State != bridge.StateUnconfigured {
		t.Fatalf("state = %v", res.State.State)
	}
	restarts := f.proc.restartsWith()
	if len(restarts) != 1 || len(restarts[0]) != 0 {
		t.Fatalf("restarts = %v, want one back-to-local restart", restarts)
	}
	if m := loadEnrollMarker(f.enr.path); m.Enrolled {
		t.Fatalf("marker survived deregister: %+v", m)
	}
	if f.stub.enrolled {
		t.Fatal("bridge still enrolled after reset")
	}
}

func TestDeregisterUnenrolledIsNoop(t *testing.T) {
	f := newEnrollFixture(t, nil)
	res := f.enr.deregister(t.Context())
	if res.Error != "" || res.State.State != bridge.StateUnconfigured {
		t.Fatalf("res = %+v", res)
	}
	if len(f.proc.restartsWith()) != 0 {
		t.Fatal("restart happened for an unenrolled bridge")
	}
}

func TestReconcileStartupHealsLocalDrift(t *testing.T) {
	f := newEnrollFixture(t, nil)
	// The settings page enrolled the bridge without flipping posture.
	f.stub.mu.Lock()
	f.stub.enrolled = true
	f.stub.mu.Unlock()
	f.enr.reconcileStartup(t.Context())
	restarts := f.proc.restartsWith()
	if len(restarts) != 1 || !argsEqual(restarts[0], []string{"--auth", "apikey"}) {
		t.Fatalf("restarts = %v", restarts)
	}
	m := loadEnrollMarker(f.enr.path)
	if !m.Enrolled || m.BridgeID != "br_1" || m.EdgeURL != "https://edge.example.com" {
		t.Fatalf("marker = %+v", m)
	}
}

func TestReconcileStartupSkipsWhenEnrolled(t *testing.T) {
	f := newEnrollFixture(t, nil)
	if err := saveEnrollMarker(f.enr.path, enrollMarker{Enrolled: true, BridgeID: "br_1"}); err != nil {
		t.Fatal(err)
	}
	f.enr.reconcileStartup(t.Context())
	if len(f.proc.restartsWith()) != 0 {
		t.Fatal("restart happened for an already-enrolled marker")
	}
}

func TestEnrollMarkerRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "marker.json")
	if m := loadEnrollMarker(path); m.Enrolled {
		t.Fatal("missing marker should read zero")
	}
	want := enrollMarker{Enrolled: true, EdgeURL: "https://edge.example.com", BridgeID: "br_1"}
	if err := saveEnrollMarker(path, want); err != nil {
		t.Fatal(err)
	}
	if got := loadEnrollMarker(path); got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
