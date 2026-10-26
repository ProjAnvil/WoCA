package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/ee/authcore"
	"github.com/projanvil/woca/internal/ee/bridge"
	"github.com/projanvil/woca/internal/ee/edgegw"
	"github.com/projanvil/woca/internal/ee/entrygw"
	"github.com/projanvil/woca/internal/ee/nativeauth"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/servefront"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/webfx"
	"github.com/projanvil/woca/internal/wocadb"
)

const (
	coreAddr   = "127.0.0.1:13713"
	edgeAddr   = "127.0.0.1:13712"
	bridgeAddr = "127.0.0.1:13711"
	coreBase   = "http://" + coreAddr
	edgeBase   = "http://" + edgeAddr
	bridgeBase = "http://" + bridgeAddr
)

// runEnterprise hosts the whole private-cloud chain in one process with
// throwaway state (mirrors internal/ee/edgegw's fake core and the edition
// serve tests): real stores on temp sqlite, the real edge handler, and a
// real bridge enrolled through the real flow. Exits non-zero if the chain
// does not become fully usable (bridge login validated at the core) within
// the startup budget.
func runEnterprise() {
	tmp, err := os.MkdirTemp("", "woca-e2e-lab-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/enterprise: tempdir: %v\n", err)
		os.Exit(1)
	}
	home := filepath.Join(tmp, "home")
	_ = os.MkdirAll(home, 0o755)
	for _, kv := range [][2]string{
		{"HOME", home},
		{"WOCA_HOME", home},
		{"WOCA_DB", filepath.Join(home, "woca.db")},
		{"WOCA_PLUGIN_ROOT", filepath.Join(home, "plugins")},
		{"WOCA_NO_BROWSER", "1"},
		// The bridge enrolls against the lab edge over plain HTTP (the
		// dev-edge posture WOCA_EDGE_INSECURE gates).
		{"WOCA_EDGE_URL", edgeBase},
		{"WOCA_EDGE_INSECURE", "1"},
	} {
		if err := os.Setenv(kv[0], kv[1]); err != nil {
			fmt.Fprintf(os.Stderr, "lab/enterprise: set %s: %v\n", kv[0], err)
			os.Exit(1)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- private core: real stores on temp sqlite, one listener --------
	db, err := wocadb.Open("sqlite:" + filepath.Join(tmp, "core.db"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/enterprise: core db: %v\n", err)
		os.Exit(1)
	}
	reg, err := entrygw.NewStore(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/enterprise: entrygw store: %v\n", err)
		os.Exit(1)
	}
	keys, err := tenant.NewStore(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/enterprise: tenant store: %v\n", err)
		os.Exit(1)
	}
	authSvc, err := authcore.New(keys, db, zerolog.Nop())
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/enterprise: authcore: %v\n", err)
		os.Exit(1)
	}

	// A tenant-admin the browser tests log in with.
	user, err := keys.CreateUser(ctx, "local", "e2e-admin", "tenant-admin")
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/enterprise: create user: %v\n", err)
		os.Exit(1)
	}
	_, userKey, err := keys.IssueKey(ctx, "local", user.ID, "e2e-admin", "tenant-admin")
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/enterprise: issue key: %v\n", err)
		os.Exit(1)
	}
	labUserKey.Store(userKey)

	// The enrollment token the bridge consumes through the real
	// POST /gw/enroll flow (preset via env, exactly like a deployment).
	enrollToken, err := reg.IssueEnrollment(ctx, entrygw.EnrollmentTTL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/enterprise: enrollment token: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("WOCA_ENROLL_TOKEN", enrollToken); err != nil {
		fmt.Fprintf(os.Stderr, "lab/enterprise: set enrollment token: %v\n", err)
		os.Exit(1)
	}

	coreMux := http.NewServeMux()
	entrygw.NewRegistryHandler(reg).RegisterRoutes(coreMux)
	// The native credential machine channel (what woca-scheduler mounts
	// beside authcore on its gateway listener) — the activate endpoint
	// the native client's connect flow posts to.
	coreMux.Handle("/gw/native/", nativeauth.MachineHandler(nativeauth.New(keys, tenant.NewAPIKeyProvider(keys))))
	coreMux.Handle("/gw/auth/", authcore.Handler(authSvc, func(string, string, string) {}))
	coreMux.HandleFunc("GET /gw/config", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"lab-gen-1","settings":{}}`))
	})
	// A never-actually-used OpenAI-compatible endpoint so the agent build
	// has a provider shape (login-surface tests never start runs).
	coreMux.HandleFunc("/fake-llm/", fakeLLM)

	coreLn, err := net.Listen("tcp", coreAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/enterprise: core listen: %v\n", err)
		os.Exit(1)
	}
	coreSrv := &http.Server{Handler: coreMux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = coreSrv.Serve(coreLn) }()
	defer shutdown(coreSrv)

	// ---- edge: the real handler on the public entry port --------------
	edge := edgegw.New(edgegw.Config{ServiceURL: coreBase, ConfigURL: coreBase}, zerolog.Nop())
	router := webfx.New(webfx.Options{Logger: zerolog.Nop(), Grace: 30 * time.Second})
	router.UseDefaults()
	router.Handle("/", edge.Handler())
	edgeLn, err := net.Listen("tcp", edgeAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/enterprise: edge listen: %v\n", err)
		os.Exit(1)
	}
	edgeSrv := &http.Server{Handler: router.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = edgeSrv.Serve(edgeLn) }()
	defer shutdown(edgeSrv)

	// ---- bridge: the exact servefront edition cmd/woca-bridge serves --
	if err := seedSettings(filepath.Join(home, "woca.db"), [2]string{"memory-engine", "none"}); err != nil {
		fmt.Fprintf(os.Stderr, "lab/enterprise: seed settings: %v\n", err)
		os.Exit(1)
	}
	state := &bridge.ServeState{}
	state.AuthMode.Store("apikey")
	opts := agentbuild.Options{
		Provider: "openai",
		Model:    "gpt-test",
		APIKey:   "test-key",
		BaseURL:  coreBase + "/fake-llm",
		Workdir:  filepath.Join(tmp, "work"),
		LogLevel: "warn",
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- servefront.ServeWithEdition(ctx, opts, bridgeAddr, true, "apikey", state.Edition())
	}()

	// ---- readiness: the chain must actually work, not just listen ------
	bridgeID, err := waitForChain()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/enterprise: %v\n", err)
		stop()
		_ = os.RemoveAll(tmp)
		os.Exit(1)
	}

	facts := map[string]string{
		"bridgeBase": bridgeBase,
		"edgeBase":   edgeBase,
		"userKey":    userKey,
		"bridgeID":   bridgeID,
	}
	factsJSON, _ := json.Marshal(facts)
	_ = os.MkdirAll(filepath.Join("e2e", ".runtime"), 0o755)
	if err := os.WriteFile(filepath.Join("e2e", ".runtime", "lab.json"), factsJSON, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "lab/enterprise: write facts: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("lab/enterprise ready: bridge=%s edge=%s bridge_id=%s\n", bridgeBase, edgeBase, bridgeID)

	<-ctx.Done()
	stop()
	// ServeWithEdition traps SIGTERM itself; give it a grace window.
	select {
	case <-serveDone:
	case <-time.After(5 * time.Second):
	}
	_ = os.RemoveAll(tmp)
}

// waitForChain blocks until every form under test is actually usable:
// bridge health, the bridge enrolled and live in the edge inventory
// (single-bridge auto-routing), and a real login validated end-to-end at
// the core through the bridge's machine channel.
func waitForChain() (string, error) {
	var lastErr error
	deadline := time.Now().Add(120 * time.Second)

	up := func(url string) bool {
		resp, err := http.Get(url)
		if err != nil {
			lastErr = err
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("%s = %d", url, resp.StatusCode)
			return false
		}
		return true
	}

	bridgeID := atomic.Value{}
	for time.Now().Before(deadline) {
		if up(bridgeBase + "/api/health") {
			if id := liveBridgeID(edgeBase); id != "" {
				bridgeID.Store(id)
				break
			} else {
				lastErr = fmt.Errorf("edge inventory does not list the bridge yet")
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	if bridgeID.Load() == nil {
		return "", fmt.Errorf("chain did not come up: %v", lastErr)
	}

	// The strongest readiness signal: a login that travels bridge → edge
	// → core and back with a session token.
	for time.Now().Before(deadline) {
		if ok, err := probeLogin(labUserKey.Load().(string)); ok {
			return bridgeID.Load().(string), nil
		} else {
			lastErr = err
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "", fmt.Errorf("bridge login never succeeded: %v", lastErr)
}

// The user key is fixed at mint time; readiness probes read it back.
var labUserKey atomic.Value // string

func probeLogin(key string) (bool, error) {
	resp, err := http.Post(bridgeBase+"/api/auth/login", "application/json",
		strings.NewReader(`{"api_key":"`+key+`"}`))
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.Token == "" {
		return false, fmt.Errorf("login = %d (token %q)", resp.StatusCode, out.Token)
	}
	return true, nil
}

// liveBridgeID returns the first live bridge from the edge's public
// inventory, or "" while the bridge is not enrolled/heartbeating yet.
func liveBridgeID(edge string) string {
	resp, err := http.Get(edge + "/api/edge/bridges")
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var bridges []struct {
		BridgeID string `json:"bridge_id"`
		Live     bool   `json:"live"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&bridges); err != nil {
		return ""
	}
	for _, b := range bridges {
		if b.Live {
			return b.BridgeID
		}
	}
	return ""
}

func seedSettings(path string, pairs ...[2]string) error {
	st, err := settings.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	cfg := st.Load()
	for _, p := range pairs {
		if err := settings.Apply(&cfg, p[0], p[1]); err != nil {
			return err
		}
	}
	return st.Save(cfg)
}

// fakeLLM answers one OpenAI-style SSE round; login-surface tests never
// reach it, but the agent build wants a provider endpoint shape.
func fakeLLM(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	chunk := func(delta map[string]any, finish any) {
		b, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-lab", "object": "chat.completion.chunk",
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		})
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	chunk(map[string]any{"role": "assistant", "content": "lab"}, nil)
	chunk(map[string]any{}, "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
