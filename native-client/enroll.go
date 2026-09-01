package main

// enroll.go is the embedded bridge's private-cloud enrollment flow (the
// connect page's enrollment view). It is deliberately wails-side
// orchestration over the bridge's OWN owner surfaces — zero changes to
// woca-bridge or woca-edge:
//
//  1. the bridge's cloud-access API (PUT/GET /api/cloud-access, writable
//     without credentials while it runs in local-trust posture — the
//     machine owner IS the admin) performs the machine-plane enrollment;
//  2. the tunnel red line (R1: a local-trust bridge must not become
//     publicly drivable) keys the tunnel to the auth posture, so the flow
//     restarts the embedded process with --auth apikey once enrolled;
//  3. the user-plane login then rides the EXISTING edge connect path
//     (Connect(edge, key, bridge_id) → /api/native/activate at the edge),
//     which keeps profiles, the session vault, and the keychain untouched.
//
// The posture marker is native-side state (a JSON file next to the
// device key): the bridge's /api/status is anonymous-readable only in
// local posture, so the app cannot probe an apikey bridge for its
// enrollment state — the app owns the posture, so the app records it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/projanvil/woca/internal/platform/machineid"
	"github.com/projanvil/woca/native-client/internal/bridge"
	"github.com/projanvil/woca/native-client/internal/bridgeproc"
)

// bridgeProcOrNil avoids a typed-nil interface (a nil *Manager reaching
// the interface methods would panic on its mutex).
func bridgeProcOrNil(m *bridgeproc.Manager) bridgeProcess {
	if m == nil {
		return nil
	}
	return m
}

// Enrollment failure classes — stable tokens the frontend maps to copy;
// "" means success. All of them are terminal per attempt: the flow never
// silently downgrades (no "enroll anyway" fallback).
const (
	enrollErrEdge   = "edge_unreachable"
	enrollErrWrite  = "bridge_write"
	enrollErrToken  = "enroll_token"
	enrollErrFlip   = "bridge_restart"
	enrollErrKey    = "user_key"
	enrollErrAbsent = "enroll_unavailable"
)

// enrollMarker is the persisted posture state. Written on enrollment
// success and deregistration; read at startup to decide the spawn args.
type enrollMarker struct {
	Enrolled bool   `json:"enrolled"`
	EdgeURL  string `json:"edge_url,omitempty"`
	BridgeID string `json:"bridge_id,omitempty"`
}

// bridgeProcess is the process seam the orchestrator drives (the
// production bridgeproc.Manager; tests inject a fake).
type bridgeProcess interface {
	BaseURL() (string, error)
	RestartWith(ctx context.Context, extraArgs ...string) (string, error)
}

// enrollConnect is the user-plane login seam (the App.Connect binding:
// activation, profile, vault, and keychain all ride it).
type enrollConnect func(ctx context.Context, input bridge.ConnectionInput) (bridge.BootstrapState, error)

type enroller struct {
	proc    bridgeProcess
	connect enrollConnect
	// onBase re-publishes the embedded base URL after a posture restart
	// (the port is randomized per spawn).
	onBase func(baseURL string)
	// path is the marker file, next to the native database.
	path string
	// Poll pacing (production defaults in newEnroller; tests shrink).
	enrollWait time.Duration
	liveWait   time.Duration
	pollStep   time.Duration
	client     *http.Client
}

func newEnroller(proc bridgeProcess, connect enrollConnect, onBase func(string), markerPath string) *enroller {
	return &enroller{
		proc:       proc,
		connect:    connect,
		onBase:     onBase,
		path:       markerPath,
		enrollWait: 30 * time.Second,
		liveWait:   30 * time.Second,
		pollStep:   1500 * time.Millisecond,
		client:     &http.Client{Timeout: 15 * time.Second},
	}
}

func enrollMarkerPath(dataDir string) string {
	return filepath.Join(dataDir, "embedded-enroll.json")
}

func loadEnrollMarker(path string) enrollMarker {
	var m enrollMarker
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	return m
}

func saveEnrollMarker(path string, m enrollMarker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// enrolled reports the marker's view (startup wiring reads this before
// the bridge exists; the connect page reads it via the binding).
func (e *enroller) marker() enrollMarker {
	if e == nil || e.path == "" {
		return enrollMarker{}
	}
	return loadEnrollMarker(e.path)
}

// EnrollEmbeddedInput mirrors the three-field enrollment form.
type EnrollEmbeddedInput struct {
	EdgeURL         string `json:"edge_url"`
	EnrollmentToken string `json:"enrollment_token"`
	APIKey          string `json:"api_key"`
}

// EnrollResult carries the flow outcome. Error holds a class token (""
// on success) rather than rejecting: a user-key failure still leaves the
// machine enrolled, and the frontend needs both facts to route to the
// sign-in view.
type EnrollResult struct {
	State    bridge.BootstrapState `json:"state"`
	BridgeID string                `json:"bridge_id,omitempty"`
	Error    string                `json:"error,omitempty"`
}

// enroll runs the full flow: precheck → write → poll enrolled → posture
// flip → wait for the tunnel → login through the edge.
func (e *enroller) enroll(ctx context.Context, input EnrollEmbeddedInput) EnrollResult {
	if e == nil || e.proc == nil {
		return EnrollResult{Error: enrollErrAbsent}
	}
	edgeURL := strings.TrimRight(strings.TrimSpace(input.EdgeURL), "/")
	token := strings.TrimSpace(input.EnrollmentToken)
	key := strings.TrimSpace(input.APIKey)

	if edgeURL == "" || token == "" || key == "" {
		return EnrollResult{Error: enrollErrAbsent}
	}
	if parsed, err := url.Parse(edgeURL); err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return EnrollResult{Error: enrollErrEdge}
	}
	if !strings.HasPrefix(token, machineid.EnrollScheme+"_") {
		return EnrollResult{Error: enrollErrToken}
	}

	base, err := e.proc.BaseURL()
	if err != nil {
		return EnrollResult{Error: enrollErrAbsent}
	}

	// 1. The edge must answer before the one-time token is spent — this
	// class of failure (wrong URL, network) must not surface as an
	// enrollment-timeout mystery.
	if err := e.precheckEdge(ctx, edgeURL); err != nil {
		return EnrollResult{Error: enrollErrEdge}
	}

	// 2. Machine-plane write (local posture: the owner surface is
	// credential-free). Configure stores the token and restarts the
	// gateway client; enrollment itself happens inside the bridge.
	if err := e.putCloudAccess(ctx, base, edgeURL, token); err != nil {
		return EnrollResult{Error: enrollErrWrite}
	}

	// 3. Poll until the bridge reports enrolled. The bridge consumes the
	// token immediately on success and exits its enroll loop on failure —
	// a timeout here means the token was invalid, expired, or already
	// used (the three are indistinguishable client-side).
	bridgeID, err := e.pollEnrolled(ctx, base)
	if err != nil {
		return EnrollResult{Error: enrollErrToken}
	}

	// 4. Posture flip: the tunnel only runs under --auth apikey (R1), and
	// only a tunnel-serving bridge is selectable at the edge. A failed
	// flip rolls back to local so the machine stays usable; the server
	// side keeps the enrollment, so a retry needs no fresh token.
	newBase, err := e.proc.RestartWith(ctx, "--auth", "apikey")
	if err != nil {
		_, _ = e.proc.RestartWith(context.WithoutCancel(ctx))
		return EnrollResult{Error: enrollErrFlip, BridgeID: bridgeID}
	}
	if e.onBase != nil {
		e.onBase(newBase)
	}

	// 5. The edge inventory must list the bridge as live — proof the
	// tunnel actually came up before the login step rides it.
	if err := e.waitBridgeLive(ctx, edgeURL, bridgeID); err != nil {
		return EnrollResult{Error: enrollErrFlip, BridgeID: bridgeID}
	}

	// 6. Persist the posture BEFORE the login attempt: a rejected key is
	// a user-plane failure on an enrolled machine, not a rollback trigger.
	if err := saveEnrollMarker(e.path, enrollMarker{Enrolled: true, EdgeURL: edgeURL, BridgeID: bridgeID}); err != nil {
		return EnrollResult{Error: enrollErrWrite, BridgeID: bridgeID}
	}

	// 7. User-plane login through the standard edge path.
	if e.connect == nil {
		return EnrollResult{BridgeID: bridgeID, Error: enrollErrKey}
	}
	state, err := e.connect(ctx, bridge.ConnectionInput{ServerURL: edgeURL, APIKey: key, BridgeID: bridgeID})
	if err != nil {
		return EnrollResult{State: state, BridgeID: bridgeID, Error: enrollErrKey}
	}
	return EnrollResult{State: state, BridgeID: bridgeID}
}

// deregister drops the embedded bridge out of its private cloud: back to
// local posture, wipe the local identity (the server-side revocation
// stays the admin's action), clear the marker. This is also the recovery
// path for a bridge whose identity the core revoked — the wails owner
// can always re-enter the credential-free owner surface by dropping the
// process back to local posture.
func (e *enroller) deregister(ctx context.Context) EnrollResult {
	if e == nil || e.proc == nil {
		return EnrollResult{Error: enrollErrAbsent}
	}
	if !e.marker().Enrolled {
		return EnrollResult{State: bridge.BootstrapState{State: bridge.StateUnconfigured}}
	}
	if _, err := e.proc.BaseURL(); err != nil {
		return EnrollResult{Error: enrollErrAbsent}
	}
	newBase, err := e.proc.RestartWith(ctx)
	if err != nil {
		return EnrollResult{Error: enrollErrFlip}
	}
	if e.onBase != nil {
		e.onBase(newBase)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, newBase+"/api/cloud-access/reset", nil)
	if err != nil {
		return EnrollResult{Error: enrollErrWrite}
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return EnrollResult{Error: enrollErrWrite}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return EnrollResult{Error: enrollErrWrite}
	}
	if err := saveEnrollMarker(e.path, enrollMarker{}); err != nil {
		return EnrollResult{Error: enrollErrWrite}
	}
	return EnrollResult{State: bridge.BootstrapState{State: bridge.StateUnconfigured}}
}

// reconcileStartup heals posture drift: a bridge enrolled through the
// settings page (no posture flip) would be enrolled-but-local — dead at
// the edge (R1). Local posture answers /api/status anonymously, so the
// drift is provable exactly where it can occur; an enrolled marker
// already implies the apikey spawn args and needs no probe.
func (e *enroller) reconcileStartup(ctx context.Context) {
	if e == nil || e.proc == nil {
		return
	}
	if e.marker().Enrolled {
		return
	}
	base, err := e.proc.BaseURL()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	st, err := fetchEmbeddedStatus(ctx, e.client, base)
	if err != nil || !st.Cloud.Enrolled || st.Cloud.BridgeID == "" {
		return
	}
	newBase, err := e.proc.RestartWith(ctx, "--auth", "apikey")
	if err != nil {
		return
	}
	if e.onBase != nil {
		e.onBase(newBase)
	}
	_ = saveEnrollMarker(e.path, enrollMarker{Enrolled: true, EdgeURL: st.Cloud.URL, BridgeID: st.Cloud.BridgeID})
}

// embeddedStatus is the /api/status shape the reconciliation reads
// (subset — the payload carries more the flow does not need).
type embeddedStatus struct {
	Auth  string `json:"auth"`
	Cloud struct {
		Enrolled bool   `json:"enrolled"`
		URL      string `json:"url"`
		BridgeID string `json:"bridge_id"`
	} `json:"cloud"`
}

func fetchEmbeddedStatus(ctx context.Context, client *http.Client, base string) (embeddedStatus, error) {
	var st embeddedStatus
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/status", nil)
	if err != nil {
		return st, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("embedded status: %s", resp.Status)
	}
	return st, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st)
}

// precheckEdge verifies the access point answers its pre-auth inventory.
func (e *enroller) precheckEdge(ctx context.Context, edgeURL string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, edgeURL+"/api/edge/bridges", nil)
	if err != nil {
		return err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("edge returned " + resp.Status)
	}
	return nil
}

// putCloudAccess writes the enrollment settings on the embedded bridge.
func (e *enroller) putCloudAccess(ctx context.Context, base, edgeURL, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"url": edgeURL, "enrollment_token": token})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/api/cloud-access", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("cloud-access returned " + resp.Status)
	}
	return nil
}

// pollEnrolled waits for the bridge to finish enrolling and returns the
// assigned bridge id.
func (e *enroller) pollEnrolled(ctx context.Context, base string) (string, error) {
	deadline := time.Now().Add(e.enrollWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		id, enrolled, err := embeddedEnrollmentState(ctx, e.client, base)
		if err == nil && enrolled {
			return id, nil
		}
		time.Sleep(e.pollStep)
	}
	return "", errors.New("enrollment did not complete")
}

func embeddedEnrollmentState(ctx context.Context, client *http.Client, base string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/cloud-access", nil)
	if err != nil {
		return "", false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false, errors.New("cloud-access returned " + resp.Status)
	}
	var st struct {
		Enrolled bool   `json:"enrolled"`
		BridgeID string `json:"bridge_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return "", false, err
	}
	return st.BridgeID, st.Enrolled, nil
}

// waitBridgeLive polls the edge's pre-auth inventory until the bridge
// reports live — the tunnel-serving proof.
func (e *enroller) waitBridgeLive(ctx context.Context, edgeURL, bridgeID string) error {
	deadline := time.Now().Add(e.liveWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		live, err := edgeBridgeLive(ctx, e.client, edgeURL, bridgeID)
		if err == nil && live {
			return nil
		}
		time.Sleep(e.pollStep)
	}
	return errors.New("bridge did not come online at the edge")
}

func edgeBridgeLive(ctx context.Context, client *http.Client, edgeURL, bridgeID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, edgeURL+"/api/edge/bridges", nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, errors.New("edge returned " + resp.Status)
	}
	var bridges []EdgeBridge
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&bridges); err != nil {
		return false, err
	}
	for _, b := range bridges {
		if b.BridgeID == bridgeID {
			return b.Live, nil
		}
	}
	return false, nil
}
