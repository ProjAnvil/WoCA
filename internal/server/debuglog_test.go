package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/obslog"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tools"
)

func newObsTestServer(t *testing.T) (*agent.Agent, *obslog.Store, *httptest.Server, string) {
	t.Helper()
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("ok")))
	obs := obslog.New(t.TempDir(), false)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
		Obs:            obs,
	})
	if err != nil {
		t.Fatal(err)
	}
	dbPath := t.TempDir() + "/s.db"
	st, err := settings.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(), Settings: st})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return a, obs, ts, dbPath
}

func TestSettingsAPITogglesDebugLogLive(t *testing.T) {
	a, obs, ts, _ := newObsTestServer(t)

	// GET reflects the default-off state and the log dir.
	resp, err := http.Get(ts.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		DebugLog bool   `json:"debug_log"`
		LogDir   string `json:"log_dir"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.DebugLog {
		t.Fatal("debug_log should default to false")
	}
	if got.LogDir == "" {
		t.Fatal("log_dir should expose the obs root")
	}

	// PUT flips it live (no restart): the store is enabled immediately.
	body := strings.NewReader(`{"debug_log":true}`)
	put, err := http.NewRequest(http.MethodPut, ts.URL+"/api/settings", body)
	if err != nil {
		t.Fatal(err)
	}
	putResp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d", putResp.StatusCode)
	}
	if !obs.Enabled() {
		t.Fatal("obs store not enabled after PUT")
	}
	_ = a

	// ... and off again.
	put, _ = http.NewRequest(http.MethodPut, ts.URL+"/api/settings", strings.NewReader(`{"debug_log":false}`))
	putResp, err = http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	putResp.Body.Close()
	if obs.Enabled() {
		t.Fatal("obs store not disabled after second PUT")
	}
}

func TestDebugLogDynamicReloadPerRun(t *testing.T) {
	_, obs, ts, dbPath := newObsTestServer(t)
	if obs.Enabled() {
		t.Fatal("precondition: obs should start disabled")
	}

	// An out-of-band write the way another process would flip the setting:
	// a second settings handle on the same database file.
	st2, err := settings.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	c := st2.Load()
	c.DebugLog = true
	if err := st2.Save(c); err != nil {
		t.Fatal(err)
	}

	// Starting a run refreshes from the DB before the first model call, so
	// the cross-process toggle applies dynamically.
	go func() {
		_, _ = http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader(`{"prompt":"hi"}`))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !obs.Enabled() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !obs.Enabled() {
		t.Fatal("out-of-band debug-log=true was not picked up at run start")
	}
}
