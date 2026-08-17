package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/rs/zerolog"
)

func TestHandleStats(t *testing.T) {
	a := newTestAgent(t) // server_test.go:32
	s := New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir()})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		ActiveRuns         int            `json:"active_runs"`
		QueuedRuns         int            `json:"queued_runs"`
		SessionQueueDepths map[string]int `json:"session_queue_depths"`
		Teammates          int            `json:"teammates"`
		BackgroundTasks    int            `json:"background_tasks"`
		SSEDropped         int64          `json:"sse_dropped"`
		LSP                struct {
			Active int `json:"active"`
			Broken int `json:"broken"`
		} `json:"lsp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ActiveRuns != 0 || body.QueuedRuns != 0 {
		t.Fatalf("body = %+v, want zeroed counters on idle server", body)
	}
}
