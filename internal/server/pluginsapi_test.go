package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/pluginrt"
	"github.com/projanvil/woca/internal/settings"
)

// newPluginsServer builds a Server with a temp settings DB and the optional
// plugin hooks.
func newPluginsServer(t *testing.T, statuses func([]string) []pluginrt.Status, install func(name string, reinstall bool) error) *httptest.Server {
	t.Helper()
	st, err := settings.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := Config{
		Static:   fstest.MapFS{},
		Logger:   zerolog.Nop(),
		Settings: st,
	}
	if statuses != nil {
		cfg.Plugins = statuses
	}
	if install != nil {
		cfg.InstallPlugin = func(_ context.Context, name string, reinstall bool) error { return install(name, reinstall) }
	}
	ts := httptest.NewServer(New(cfg).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func getPlugins(t *testing.T, ts *httptest.Server) pluginrt.Status {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/plugins")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/plugins: HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Plugins []pluginrt.Status `json:"plugins"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Plugins) == 0 {
		t.Fatal("no plugins listed")
	}
	return payload.Plugins[0]
}

func TestPluginsCatalogWithoutManager(t *testing.T) {
	ts := newPluginsServer(t, nil, nil)
	first := getPlugins(t, ts)
	if first.Name == "" || first.Kind == "" {
		t.Fatalf("catalog entry incomplete: %+v", first)
	}
	if first.State != pluginrt.StateNotInstalled {
		t.Fatalf("catalog-only state = %q, want not_installed", first.State)
	}
	if first.Enabled {
		t.Fatal("nothing is enabled by default")
	}
}

func TestPluginsPutSelection(t *testing.T) {
	ts := newPluginsServer(t, nil, nil)

	// Unknown names are rejected rather than silently stored.
	resp, err := http.NewRequest(http.MethodPut, ts.URL+"/api/plugins", strings.NewReader(`{"enabled":["nope"]}`))
	if err != nil {
		t.Fatal(err)
	}
	putResp, err := http.DefaultClient.Do(resp)
	if err != nil {
		t.Fatal(err)
	}
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown plugin: HTTP %d, want 400", putResp.StatusCode)
	}

	resp, _ = http.NewRequest(http.MethodPut, ts.URL+"/api/plugins", strings.NewReader(`{"enabled":["pdf"]}`))
	putResp, err = http.DefaultClient.Do(resp)
	if err != nil {
		t.Fatal(err)
	}
	defer putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(putResp.Body)
		t.Fatalf("PUT pdf: HTTP %d: %s", putResp.StatusCode, body)
	}

	var list struct {
		Plugins []pluginrt.Status `json:"plugins"`
	}
	got, err := http.Get(ts.URL + "/api/plugins")
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	_ = json.NewDecoder(got.Body).Decode(&list)
	for _, p := range list.Plugins {
		if p.Name == "pdf" && !p.Enabled {
			t.Fatal("pdf not enabled after PUT")
		}
	}
}

func TestPluginsInstallEndpoint(t *testing.T) {
	// No hook: 503.
	ts := newPluginsServer(t, nil, nil)
	resp, err := http.Post(ts.URL+"/api/plugins/pdf/install", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("install without hook: HTTP %d, want 503", resp.StatusCode)
	}

	// Unknown plugin: 404 even with the hook present.
	var requested string
	var reinstalled bool
	ts2 := newPluginsServer(t, nil, func(name string, reinstall bool) error {
		requested = name
		reinstalled = reinstall
		return nil
	})
	resp, err = http.Post(ts2.URL+"/api/plugins/ghost/install", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("install unknown: HTTP %d, want 404", resp.StatusCode)
	}

	// Happy path kicks the install (202) without running it inline.
	resp, err = http.Post(ts2.URL+"/api/plugins/pdf/install", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("install pdf: HTTP %d, want 202", resp.StatusCode)
	}
	if requested != "pdf" || reinstalled {
		t.Fatalf("install hook saw %q reinstall=%v, want pdf reinstall=false", requested, reinstalled)
	}

	// The /reinstall variant flips the flag (upgrade path).
	resp, err = http.Post(ts2.URL+"/api/plugins/pdf/reinstall", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("reinstall pdf: HTTP %d, want 202", resp.StatusCode)
	}
	if !reinstalled {
		t.Fatal("reinstall flag not relayed")
	}
}

func TestPluginsStatusHook(t *testing.T) {
	ts := newPluginsServer(t, func(enabled []string) []pluginrt.Status {
		return []pluginrt.Status{{
			Name: "browser", Kind: "golang", Enabled: true,
			State: pluginrt.StateInstalling, Log: []string{"downloading…"},
		}}
	}, nil)
	resp, err := http.Get(ts.URL + "/api/plugins")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload struct {
		Plugins []pluginrt.Status `json:"plugins"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Plugins) != 1 || payload.Plugins[0].State != pluginrt.StateInstalling {
		t.Fatalf("status hook output not relayed: %+v", payload.Plugins)
	}
}

func TestPluginsDegradedAndValidation(t *testing.T) {
	// Without a settings store the catalog still lists; the enabled flags
	// just stay empty.
	bare := New(Config{Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(bare.Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/api/plugins")
	if err != nil {
		t.Fatal(err)
	}
	var payload pluginsResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(payload.Plugins) == 0 {
		t.Fatal("catalog should list without a settings store")
	}

	// PUT without a settings store: 503.
	put, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/plugins", strings.NewReader(`{"enabled":["pdf"]}`))
	putResp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("PUT without settings: HTTP %d, want 503", putResp.StatusCode)
	}

	// A status hook returning nothing yields an empty list, not null.
	empty := newPluginsServer(t, func(enabled []string) []pluginrt.Status { return nil }, nil)
	resp2, err := http.Get(empty.URL + "/api/plugins")
	if err != nil {
		t.Fatal(err)
	}
	var p2 pluginsResponse
	if err := json.NewDecoder(resp2.Body).Decode(&p2); err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if p2.Plugins == nil {
		t.Fatal("hook with no plugins must serialize as [], not null")
	}

	// Malformed PUT bodies: 400.
	ts3 := newPluginsServer(t, nil, nil)
	put3, _ := http.NewRequest(http.MethodPut, ts3.URL+"/api/plugins", strings.NewReader(`{bad`))
	badResp, err := http.DefaultClient.Do(put3)
	if err != nil {
		t.Fatal(err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT bad JSON: HTTP %d, want 400", badResp.StatusCode)
	}
}

func TestPluginsPutWithAgentAndInstallError(t *testing.T) {
	// A PUT with an agent wired refreshes its tool binding inline.
	st, err := settings.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := New(Config{
		Agent:    newTestAgent(t),
		Static:   fstest.MapFS{},
		Logger:   zerolog.Nop(),
		Settings: st,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	put, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/plugins", strings.NewReader(`{"enabled":["pdf"]}`))
	resp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT with agent: HTTP %d, want 200", resp.StatusCode)
	}

	// A failing install hook is a 409 (conflict with the runtime state).
	conflict := newPluginsServer(t, nil, func(name string, reinstall bool) error {
		return errors.New("already installing")
	})
	postResp, err := http.Post(conflict.URL+"/api/plugins/pdf/install", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusConflict {
		t.Fatalf("install error: HTTP %d, want 409", postResp.StatusCode)
	}
}

func TestPluginsPutSaveFailureAfterClose(t *testing.T) {
	st, err := settings.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Static: fstest.MapFS{}, Logger: zerolog.Nop(), Settings: st})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	// A closed database fails the save: 500 and the selection unchanged.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	put, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/plugins", strings.NewReader(`{"enabled":["pdf"]}`))
	resp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("PUT after close: HTTP %d, want 500", resp.StatusCode)
	}
}
