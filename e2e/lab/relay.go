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
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/relay"
	"github.com/projanvil/woca/internal/relayclient"
	"github.com/projanvil/woca/internal/servefront"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/webfx"
	webdist "github.com/projanvil/woca/web"
)

const (
	relayAddr     = "127.0.0.1:13715"
	relayWocaAddr = "127.0.0.1:13716"
	relayBase     = "http://" + relayAddr
	relayWocaBase = "http://" + relayWocaAddr
)

// runRelay hosts the personal remote-relay chain in one process with
// throwaway state — the in-process twin of ci/e2e-relay.sh: a real
// woca-relay (keystore + auth + tunnel + SPA, with the revocation sweep
// running) and the real personal serve behind it, connected the same way
// `woca relay connect` persists coordinates. Two keys are minted: an
// active one for the browser login and one revoked up front, so the UI's
// revoked-notice path is coverable.
func runRelay() {
	tmp, err := os.MkdirTemp("", "woca-e2e-relay-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/relay: tempdir: %v\n", err)
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
	} {
		if err := os.Setenv(kv[0], kv[1]); err != nil {
			fmt.Fprintf(os.Stderr, "lab/relay: set %s: %v\n", kv[0], err)
			os.Exit(1)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- relay side: the real public surface ---------------------------
	relayDB := filepath.Join(tmp, "relay.db")
	keys, err := relay.OpenKeyStore(relayDB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/relay: keystore: %v\n", err)
		os.Exit(1)
	}
	_, activeKey, err := keys.CreateKey(ctx, "e2e-relay")
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/relay: create key: %v\n", err)
		os.Exit(1)
	}
	_, revokedKey, err := keys.CreateKey(ctx, "e2e-revoked")
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/relay: create revoked key: %v\n", err)
		os.Exit(1)
	}
	if _, ok, err := keys.RevokeKey(ctx, "e2e-revoked"); err != nil || !ok {
		fmt.Fprintf(os.Stderr, "lab/relay: pre-revoke: ok=%v err=%v\n", ok, err)
		os.Exit(1)
	}

	rl := relay.NewRelay(keys, webdist.DistFS(), false, zerolog.Nop())
	auditLog, err := relay.NewAuditLog(relayDB + ".audit.log")
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/relay: audit log: %v\n", err)
		os.Exit(1)
	}
	revoker := rl.AttachRevocation(auditLog)
	go revoker.Watch(ctx, time.Second)

	router := webfx.New(webfx.Options{Logger: zerolog.Nop(), Grace: 40 * time.Second, ReadHeaderTimeout: 15 * time.Second})
	router.UseDefaults()
	router.Handle("/", rl.Handler())
	relayLn, err := net.Listen("tcp", relayAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/relay: relay listen: %v\n", err)
		os.Exit(1)
	}
	relaySrv := &http.Server{Handler: router.Handler()}
	go func() { _ = relaySrv.Serve(relayLn) }()
	defer shutdown(relaySrv)

	// ---- personal serve side: cmd/woca's exact relay wiring ------------
	// Persist the coordinates the way `woca relay connect` does (the
	// serve path picks them up from the settings store).
	st, err := settings.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/relay: settings open: %v\n", err)
		os.Exit(1)
	}
	if err := relayclient.SaveConfig(st, relayBase, activeKey); err != nil {
		_ = st.Close()
		fmt.Fprintf(os.Stderr, "lab/relay: save config: %v\n", err)
		os.Exit(1)
	}
	_ = st.Close()

	relaySvc := relayclient.NewService()
	ed := servefront.Edition{
		Static:  webdist.DistFS(),
		Handler: relaySvc.SetHandler,
		Routes: func(mux *http.ServeMux, _ authn.Provider) {
			mux.Handle("GET /api/relay/status", relaySvc.StatusHandler())
			mux.Handle("POST /api/relay/disconnect", relaySvc.DisconnectHandler())
		},
		OnSettings: relaySvc.SetSettings,
		Start: func(ctx context.Context, _ servefront.Observability, st *settings.Store, log zerolog.Logger) {
			relaySvc.Run(ctx, st, log)
		},
	}
	opts := agentbuild.Options{Workdir: filepath.Join(tmp, "work"), LogLevel: "warn"}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- servefront.ServeWithEdition(ctx, opts, relayWocaAddr, true, "", ed)
	}()

	// ---- readiness: login through the relay and a tunneled API ---------
	if err := waitForRelayChain(activeKey); err != nil {
		fmt.Fprintf(os.Stderr, "lab/relay: %v\n", err)
		stop()
		_ = os.RemoveAll(tmp)
		os.Exit(1)
	}

	facts := map[string]string{
		"relayBase":  relayBase,
		"wocaBase":   relayWocaBase,
		"relayKey":   activeKey,
		"revokedKey": revokedKey,
	}
	factsJSON, _ := json.Marshal(facts)
	_ = os.MkdirAll(filepath.Join("e2e", ".runtime"), 0o755)
	if err := os.WriteFile(filepath.Join("e2e", ".runtime", "relay.json"), factsJSON, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "lab/relay: write facts: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("lab/relay ready: relay=%s woca=%s\n", relayBase, relayWocaBase)

	<-ctx.Done()
	stop()
	select {
	case <-serveDone:
	case <-time.After(5 * time.Second):
	}
	_ = os.RemoveAll(tmp)
}

// waitForRelayChain blocks until the relay answers health, the serve is
// up, a login succeeds with the active key, and /api/sessions answers
// through the tunnel — the full path every relay test depends on.
func waitForRelayChain(key string) error {
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
	for time.Now().Before(deadline) {
		if !up(relayBase+"/api/health") || !up(relayWocaBase+"/api/health") {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		tok, err := relayLogin(relayBase, key)
		if err != nil {
			lastErr = err
			time.Sleep(300 * time.Millisecond)
			continue
		}
		if code := tunneledGet(relayBase+"/api/sessions", tok); code == http.StatusOK {
			return nil
		} else {
			lastErr = fmt.Errorf("tunneled /api/sessions = %d", code)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("relay chain did not come up: %v", lastErr)
}

func relayLogin(base, key string) (string, error) {
	resp, err := http.Post(base+"/api/auth/login", "application/json",
		strings.NewReader(`{"api_key":"`+key+`"}`))
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.Token == "" {
		return "", fmt.Errorf("relay login = %d (token %q)", resp.StatusCode, out.Token)
	}
	return out.Token, nil
}

func tunneledGet(url, token string) int {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}
