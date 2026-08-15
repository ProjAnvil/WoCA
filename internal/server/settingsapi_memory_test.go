package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/settings"
)

// newMemorySettingsServer builds a Server whose settings store lives in a
// temp database and whose rebuildMemory hook is fn (nil disables it).
func newMemorySettingsServer(t *testing.T, fn func() error) *httptest.Server {
	t.Helper()
	st, err := settings.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := New(Config{
		Static:        fstest.MapFS{},
		Logger:        zerolog.Nop(),
		Settings:      st,
		RebuildMemory: fn,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// putSettings PUTs a JSON body and returns the status plus decoded payload.
func putSettings(t *testing.T, ts *httptest.Server, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.NewRequest(http.MethodPut, ts.URL+"/api/settings", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	putResp, err := http.DefaultClient.Do(resp)
	if err != nil {
		t.Fatal(err)
	}
	defer putResp.Body.Close()
	raw, _ := io.ReadAll(putResp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("PUT %s: non-JSON response %q", body, raw)
	}
	return putResp.StatusCode, out
}

// getSettings GETs the settings payload decoded into the requested fields.
func getSettings(t *testing.T, ts *httptest.Server) map[string]any {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("GET /api/settings: non-JSON response %q", raw)
	}
	return out
}

func TestSettingsAPIMemoryEngineRebuild(t *testing.T) {
	var calls atomic.Int64
	ts := newMemorySettingsServer(t, func() error {
		calls.Add(1)
		return nil
	})

	// Default GET exposes the memory-engine fields with normalized defaults.
	if got := getSettings(t, ts)["memory_engine"]; got != "ladym" {
		t.Fatalf("default memory_engine = %v, want ladym", got)
	}

	// Engine switch rebuilds live: 200, no restart, rebuild called once.
	status, body := putSettings(t, ts, `{"memory_engine":"none"}`)
	if status != http.StatusOK {
		t.Fatalf("PUT memory_engine=none status = %d, body %v", status, body)
	}
	if body["ok"] != true || body["restart_required"] != false {
		t.Fatalf("PUT memory_engine=none body = %v", body)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("rebuildMemory calls = %d after engine switch, want 1", n)
	}
	if got := getSettings(t, ts)["memory_engine"]; got != "none" {
		t.Fatalf("memory_engine after PUT = %v, want none", got)
	}

	// Bogus engine value is rejected before saving or rebuilding.
	if n := calls.Load(); n != 1 {
		t.Fatalf("precondition: rebuild calls = %d", n)
	}
	status, body = putSettings(t, ts, `{"memory_engine":"bogus"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("PUT memory_engine=bogus status = %d, body %v", status, body)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("rebuildMemory calls = %d after invalid engine, want 1", n)
	}
	if got := getSettings(t, ts)["memory_engine"]; got != "none" {
		t.Fatalf("memory_engine after invalid PUT = %v, want none (unchanged)", got)
	}

	// Per-run keys apply without a rebuild.
	status, body = putSettings(t, ts, `{"ladym_topk":8}`)
	if status != http.StatusOK {
		t.Fatalf("PUT ladym_topk=8 status = %d, body %v", status, body)
	}
	if body["restart_required"] != false {
		t.Fatalf("PUT ladym_topk=8 restart_required = %v", body["restart_required"])
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("rebuildMemory calls = %d after topk change, want 1", n)
	}
	if got := getSettings(t, ts)["ladym_topk"]; got != float64(8) {
		t.Fatalf("ladym_topk after PUT = %v, want 8", got)
	}

	// Out-of-range topk is a validation error.
	status, body = putSettings(t, ts, `{"ladym_topk":99}`)
	if status != http.StatusBadRequest {
		t.Fatalf("PUT ladym_topk=99 status = %d, body %v", status, body)
	}
	if got := getSettings(t, ts)["ladym_topk"]; got != float64(8) {
		t.Fatalf("ladym_topk after invalid PUT = %v, want 8 (unchanged)", got)
	}

	// Booleans persist explicitly (Save always writes them), no rebuild.
	status, body = putSettings(t, ts, `{"ladym_autoretain":false}`)
	if status != http.StatusOK {
		t.Fatalf("PUT ladym_autoretain=false status = %d, body %v", status, body)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("rebuildMemory calls = %d after autoretain change, want 1", n)
	}
	if got := getSettings(t, ts)["ladym_autoretain"]; got != false {
		t.Fatalf("ladym_autoretain after PUT = %v, want false", got)
	}

	// Embedding-provider changes rebuild the engine.
	status, body = putSettings(t, ts, `{"ladym_embedding":"hashing"}`)
	if status != http.StatusOK {
		t.Fatalf("PUT ladym_embedding=hashing status = %d, body %v", status, body)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("rebuildMemory calls = %d after embedding change, want 2", n)
	}
	if got := getSettings(t, ts)["ladym_embedding"]; got != "hashing" {
		t.Fatalf("ladym_embedding after PUT = %v, want hashing", got)
	}
}

func TestSettingsAPILadyMSecretRebuild(t *testing.T) {
	var calls atomic.Int64
	ts := newMemorySettingsServer(t, func() error {
		calls.Add(1)
		return nil
	})

	// Setting the secret stores it (masked in GET) and rebuilds.
	status, body := putSettings(t, ts, `{"ladym_embedding_api_key":"sk-x"}`)
	if status != http.StatusOK {
		t.Fatalf("PUT ladym_embedding_api_key status = %d, body %v", status, body)
	}
	if body["restart_required"] != false {
		t.Fatalf("PUT ladym_embedding_api_key restart_required = %v", body["restart_required"])
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("rebuildMemory calls = %d after secret set, want 1", n)
	}
	if got := getSettings(t, ts)["ladym_embedding_api_key_set"]; got != true {
		t.Fatalf("ladym_embedding_api_key_set after PUT = %v, want true", got)
	}

	// Present-but-empty clears the secret (and still rebuilds).
	status, body = putSettings(t, ts, `{"ladym_embedding_api_key":""}`)
	if status != http.StatusOK {
		t.Fatalf("PUT ladym_embedding_api_key='' status = %d, body %v", status, body)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("rebuildMemory calls = %d after secret clear, want 2", n)
	}
	if got := getSettings(t, ts)["ladym_embedding_api_key_set"]; got != false {
		t.Fatalf("ladym_embedding_api_key_set after clear = %v, want false", got)
	}
}

func TestSettingsAPIMemoryRebuildError(t *testing.T) {
	ts := newMemorySettingsServer(t, func() error { return errors.New("boom") })

	status, body := putSettings(t, ts, `{"memory_engine":"none"}`)
	if status != http.StatusOK {
		t.Fatalf("PUT status = %d, body %v", status, body)
	}
	if body["ok"] != true {
		t.Fatalf("ok = %v, want true (rebuild errors are reported, not fatal)", body)
	}
	if body["restart_required"] != false {
		t.Fatalf("restart_required = %v, want false", body["restart_required"])
	}
	applyErr, _ := body["apply_error"].(string)
	if !strings.Contains(applyErr, "boom") {
		t.Fatalf("apply_error = %q, want it to contain %q", applyErr, "boom")
	}
}
