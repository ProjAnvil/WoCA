package edgegw

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/ee/obs"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/webfx"
)

// Scenario-four static-surface regression (product design §5.4): the
// bridge's SPA answer is the one tunneled payload class whose body is
// produced by http.ServeContent/io.CopyN from an embedded FS instead of
// direct handler writes — the shape that once recursed through the
// webfx capture writer (fixed 2026-08-30 in 6be37a9: "browsing the
// edge's scenario-four surface killed the bridge process ... parked
// client requests returned empty bodies"). These tests drive GET /
// through the edge with the deployment's real assembly on both sides:
// the edge wrapped in its webfx + obs chain (cmd/woca-edge), the bridge
// serving servefront's exact static path (server.New over an embedded
// frontend — the same thing cmd/woca-bridge feeds webdist into Edition.
// Static — wrapped by obs.Wrap the way servefront does).

// staticTestFiles embeds a minimal SPA tree the way webdist embeds
// web/dist (an embed.FS re-rooted with fs.Sub).
//
//go:embed testdata/index.html
//go:embed testdata/assets/app.js
var staticTestFiles embed.FS

func staticTestDist() fs.FS {
	sub, err := fs.Sub(staticTestFiles, "testdata")
	if err != nil {
		panic(err)
	}
	return sub
}

// realEdgeServer mounts the edge exactly the way cmd/woca-edge serves
// it: the webfx standard chain (RequestID, Logger, Recovery) plus the
// obs HTTP span middleware around the edge router.
func realEdgeServer(t *testing.T, core *fakeCore) *httptest.Server {
	t.Helper()
	obsInst, err := obs.Setup(context.Background(), obs.FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obsInst.Shutdown(context.Background()) })
	router := webfx.New(webfx.Options{Logger: zerolog.Nop(), Grace: 40 * time.Second})
	router.UseDefaults()
	router.Use(obsInst.HTTPMiddleware())
	router.Handle("/", New(Config{ServiceURL: core.svc.URL, ConfigURL: core.cfg.URL}, zerolog.Nop()).Handler())
	ts := httptest.NewServer(router.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// realBridgeHandler mirrors servefront's bridge assembly: server.New
// over the embedded frontend (the webfx chain and the gateway auth
// pass-through for non-API paths are srv.Handler()'s own), then obs.Wrap
// at the outermost edge — the bridge hides the span middleware behind
// the Observability seam (spansOff), so only Wrap applies.
func realBridgeHandler(t *testing.T) http.Handler {
	t.Helper()
	obsInst, err := obs.Setup(context.Background(), obs.FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obsInst.Shutdown(context.Background()) })
	srv := server.New(server.Config{
		Static: staticTestDist(),
		Logger: zerolog.Nop(),
	})
	return obsInst.Wrap(srv.Handler())
}

func tunnelBody(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(body)
}

// staticStack spins one full stack: real-edge chains over the fake core,
// a bridge tunnel serving the real static path.
func staticStack(t *testing.T) *httptest.Server {
	t.Helper()
	parkPickupTimeout = 5 * time.Second
	t.Cleanup(func() { parkPickupTimeout = 30 * time.Second })
	core := newFakeCore(t)
	edge := realEdgeServer(t, core)
	access := enroll(t, core)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runTunnel(ctx, t, core, edge.URL, access, realBridgeHandler(t))
	return edge
}

// TestTunnelStaticSPA: the white-screen repro — a browser's GET /
// through the edge must receive the bridge's index.html byte for byte,
// with the file's content type, not an empty shell.
func TestTunnelStaticSPA(t *testing.T) {
	edge := staticStack(t)

	want, err := fs.ReadFile(staticTestDist(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	resp, body := tunnelBody(t, edge.URL+"/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d", resp.StatusCode)
	}
	if body != string(want) {
		t.Fatalf("GET / through the tunnel = %q, want the index body %q", body, string(want))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("GET / content-type = %q", ct)
	}

	// A real asset (non-root branch of the SPA file server) rides too.
	wantJS, err := fs.ReadFile(staticTestDist(), "assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	respJS, bodyJS := tunnelBody(t, edge.URL+"/assets/app.js")
	if respJS.StatusCode != http.StatusOK || bodyJS != string(wantJS) {
		t.Fatalf("asset through the tunnel = %d %q", respJS.StatusCode, bodyJS)
	}
}

// TestTunnelStaticSPAPinned: the direct-link shape — the bridge pin
// travels as the ?bridge= query on the document request itself.
func TestTunnelStaticSPAPinned(t *testing.T) {
	edge := staticStack(t)

	list, err := http.Get(edge.URL + "/api/edge/bridges")
	if err != nil {
		t.Fatal(err)
	}
	var bridges []struct {
		BridgeID string `json:"bridge_id"`
	}
	_ = json.NewDecoder(list.Body).Decode(&bridges)
	list.Body.Close()
	if len(bridges) != 1 {
		t.Fatalf("inventory = %+v", bridges)
	}

	want, err := fs.ReadFile(staticTestDist(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	resp, body := tunnelBody(t, edge.URL+"/?bridge="+bridges[0].BridgeID)
	if resp.StatusCode != http.StatusOK || body != string(want) {
		t.Fatalf("pinned GET / = %d %q, want the index body", resp.StatusCode, body)
	}
}

// TestTunnelStaticStackAPIRoundTrip: the same bridge stack keeps
// answering /api/* through the tunnel — the edge validates the client
// session, the bridge's anonymous health probe answers behind it.
func TestTunnelStaticStackAPIRoundTrip(t *testing.T) {
	core := newFakeCore(t)
	edge := realEdgeServer(t, core)
	access := enroll(t, core)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runTunnel(ctx, t, core, edge.URL, access, realBridgeHandler(t))
	tok := userKey(t, edge, core, "static-api-user")

	req, _ := http.NewRequest(http.MethodGet, edge.URL+"/api/health", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok"`) {
		t.Fatalf("api round-trip = %d %s", resp.StatusCode, body)
	}
}
