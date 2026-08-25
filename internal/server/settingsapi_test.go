package server

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/settings"
)

// TestSettingsAPIDegradedPaths covers the endpoints without a settings store
// and malformed update bodies.
func TestSettingsAPIDegradedPaths(t *testing.T) {
	ts := newTestServer(t, Config{})
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET without store: status %d, want 503", resp.StatusCode)
	}
	put, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/settings", strings.NewReader(`{}`))
	putResp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("PUT without store: status %d, want 503", putResp.StatusCode)
	}

	// With a store, malformed bodies are 400s.
	st, err := settings.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ts2 := newTestServer(t, Config{Settings: st})
	t.Cleanup(ts2.Close)
	put2, _ := http.NewRequest(http.MethodPut, ts2.URL+"/api/settings", strings.NewReader(`{bad`))
	badResp, err := http.DefaultClient.Do(put2)
	if err != nil {
		t.Fatal(err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT bad JSON: status %d, want 400", badResp.StatusCode)
	}
}

func TestSettingsFloatPatchAndRebuildModel(t *testing.T) {
	var modelErr = errors.New("model rebuild failed")
	st, err := settings.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ts := newTestServer(t, Config{
		Settings:     st,
		RebuildModel: func() error { return modelErr },
	})

	// Temperature exercises the float patch path end to end.
	status, body := putSettings(t, ts, `{"temperature":0.7,"model":"glm-5.3"}`)
	if status != http.StatusOK {
		t.Fatalf("PUT status = %d, body %v", status, body)
	}
	applyErr, _ := body["apply_error"].(string)
	if !strings.Contains(applyErr, "model rebuild failed") {
		t.Fatalf("apply_error = %q, want the rebuild failure", applyErr)
	}
	if got := getSettings(t, ts)["temperature"]; got != float64(0.7) {
		t.Fatalf("temperature after PUT = %v, want 0.7", got)
	}
}

func TestSettingsRebuildErrorsConcatenate(t *testing.T) {
	st, err := settings.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ts := newTestServer(t, Config{
		Settings:      st,
		RebuildModel:  func() error { return errors.New("m-err") },
		RebuildMemory: func() error { return errors.New("mem-err") },
	})

	// One request touching both a model key and a memory key: both rebuild
	// failures are reported, joined with "; ".
	status, body := putSettings(t, ts, `{"model":"glm-5.3","memory_engine":"none"}`)
	if status != http.StatusOK {
		t.Fatalf("PUT status = %d, body %v", status, body)
	}
	applyErr, _ := body["apply_error"].(string)
	if !strings.Contains(applyErr, "m-err") || !strings.Contains(applyErr, "; ") || !strings.Contains(applyErr, "mem-err") {
		t.Fatalf("apply_error = %q, want both failures joined", applyErr)
	}
}

func TestSettingsSaveFailureAfterClose(t *testing.T) {
	st, err := settings.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	ts := newTestServer(t, Config{Settings: st})
	t.Cleanup(ts.Close)

	// A closed database fails the save: 500, settings unchanged.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	put, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/settings", strings.NewReader(`{"model":"glm-5.3"}`))
	resp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("PUT after close: status %d, want 500", resp.StatusCode)
	}
}
