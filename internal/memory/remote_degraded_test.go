package memory

// Gap tests for the remote engine: default limits, breaker short-circuits,
// degraded reads, null-memory filtering, truncation, and error surfacing.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// cannedLadyM answers one configurable recall payload so dedup, null-memory
// filtering, and truncation branches are reachable.
func cannedLadyM(t *testing.T, recall any, failRecall, failList, failForget, failStats, failConsolidate bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	status := func(fail bool, w http.ResponseWriter) bool {
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return true
		}
		return false
	}
	mux.HandleFunc("POST /api/recall", func(w http.ResponseWriter, r *http.Request) {
		if status(failRecall, w) {
			return
		}
		_ = json.NewEncoder(w).Encode(recall)
	})
	mux.HandleFunc("GET /api/memories", func(w http.ResponseWriter, r *http.Request) {
		if status(failList, w) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"memories": []any{
				map[string]any{"id": "m1", "layer": "semantic", "type": "fact", "content": "c", "updated_at": 1},
				nil,
			},
			"total": 1,
		})
	})
	mux.HandleFunc("POST /api/forget", func(w http.ResponseWriter, r *http.Request) {
		status(failForget, w)
	})
	mux.HandleFunc("POST /api/stats", func(w http.ResponseWriter, r *http.Request) {
		if status(failStats, w) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_memories": 1, "by_layer": map[string]int{"semantic": 1}})
	})
	mux.HandleFunc("POST /api/consolidate", func(w http.ResponseWriter, r *http.Request) {
		status(failConsolidate, w)
	})
	mux.HandleFunc("POST /api/record_event", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "e_1"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func remoteOver(t *testing.T, url string) *RemoteEngine {
	t.Helper()
	eng, err := BuildRemote(RemoteConfig{Endpoint: url})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

// tripBreaker forces the read breaker open for the next calls.
func tripBreaker(eng *RemoteEngine) {
	eng.mu.Lock()
	eng.openUntil = time.Now().Add(time.Minute)
	eng.mu.Unlock()
}

func TestRemoteRecallDefaultsAndFiltering(t *testing.T) {
	ctx := t.Context()

	t.Run("default topK and empty workspace", func(t *testing.T) {
		f := newFakeLadyM(t)
		eng := newTestRemote(t, f)
		hits, err := eng.Recall(ctx, "q", "", 0)
		if err != nil || len(hits) == 0 {
			t.Fatalf("recall = %v %v", hits, err)
		}
		ws, _, _ := f.snapshot()
		if len(ws) == 0 || ws[0] == "" || ws[0] == "woca:local::" {
			t.Fatalf("workspace keys = %v", ws)
		}
		if _, err := eng.SearchCode(ctx, "q", "", 0); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("null and duplicate memories are dropped", func(t *testing.T) {
		payload := map[string]any{"results": []any{
			map[string]any{"memory": nil, "score": 0.5},
			map[string]any{"memory": map[string]any{"id": "dup", "layer": "L2", "type": "fact", "content": "c", "updated_at": 1}, "score": 0.9},
			map[string]any{"memory": map[string]any{"id": "dup", "layer": "L2", "type": "fact", "content": "c", "updated_at": 1}, "score": 0.8},
		}}
		srv := cannedLadyM(t, payload, false, false, false, false, false)
		eng := remoteOver(t, srv.URL)
		hits, err := eng.Recall(ctx, "q", "ws", 5)
		if err != nil || len(hits) != 1 || hits[0].ID != "dup" {
			t.Fatalf("dedup recall = %v %v", hits, err)
		}
	})

	t.Run("results truncate to topK", func(t *testing.T) {
		payload := map[string]any{"results": []any{
			map[string]any{"memory": map[string]any{"id": "a", "content": "c", "updated_at": 1}, "score": 0.9},
			map[string]any{"memory": map[string]any{"id": "b", "content": "c", "updated_at": 1}, "score": 0.8},
			map[string]any{"memory": map[string]any{"id": "c", "content": "c", "updated_at": 1}, "score": 0.7},
		}}
		srv := cannedLadyM(t, payload, false, false, false, false, false)
		eng := remoteOver(t, srv.URL)
		hits, err := eng.Recall(ctx, "q", "ws", 2)
		if err != nil || len(hits) != 2 || hits[0].ID != "a" {
			t.Fatalf("truncated recall = %v %v", hits, err)
		}
	})

	t.Run("null memory in code search", func(t *testing.T) {
		payload := map[string]any{"results": []any{map[string]any{"memory": nil, "score": 1}}}
		srv := cannedLadyM(t, payload, false, false, false, false, false)
		eng := remoteOver(t, srv.URL)
		hits, err := eng.SearchCode(ctx, "q", "ws", 5)
		if err != nil || len(hits) != 0 {
			t.Fatalf("code search = %v %v", hits, err)
		}
	})
}

func TestRemoteBreakerShortCircuitsReads(t *testing.T) {
	f := newFakeLadyM(t)
	eng := newTestRemote(t, f)
	tripBreaker(eng)
	ctx := t.Context()

	if hits, err := eng.Recall(ctx, "q", "ws", 5); err != nil || hits != nil {
		t.Fatalf("open-breaker recall = %v %v", hits, err)
	}
	if hits, err := eng.SearchCode(ctx, "q", "ws", 5); err != nil || hits != nil {
		t.Fatalf("open-breaker search = %v %v", hits, err)
	}
	if hits, err := eng.List(ctx, "ws", "", 5, 0); err != nil || hits != nil {
		t.Fatalf("open-breaker list = %v %v", hits, err)
	}
	if st, err := eng.Stats(ctx, "ws"); err != nil || st.Total != 0 {
		t.Fatalf("open-breaker stats = %v %v", st, err)
	}
}

func TestRemoteDegradedAndFailingPaths(t *testing.T) {
	ctx := t.Context()

	t.Run("search degrades to empty on failure", func(t *testing.T) {
		srv := cannedLadyM(t, nil, true, false, false, false, false)
		eng := remoteOver(t, srv.URL)
		hits, err := eng.SearchCode(ctx, "q", "ws", 5)
		if err != nil || hits != nil {
			t.Fatalf("degraded search = %v %v", hits, err)
		}
	})

	t.Run("list error surfaces and null entries skip", func(t *testing.T) {
		srv := cannedLadyM(t, nil, false, true, false, false, false)
		eng := remoteOver(t, srv.URL)
		if _, err := eng.List(ctx, "ws", "", 5, 0); err == nil {
			t.Fatal("failing list should surface an error")
		}
		srvOK := cannedLadyM(t, nil, false, false, false, false, false)
		engOK := remoteOver(t, srvOK.URL)
		hits, err := engOK.List(ctx, "ws", "", 5, 0)
		if err != nil || len(hits) != 1 || hits[0].ID != "m1" {
			t.Fatalf("list = %v %v", hits, err)
		}
	})

	t.Run("forget stats consolidate errors surface", func(t *testing.T) {
		srv := cannedLadyM(t, nil, false, false, true, true, true)
		eng := remoteOver(t, srv.URL)
		if err := eng.Forget(ctx, "m1"); err == nil {
			t.Fatal("failing forget should surface")
		}
		if _, err := eng.Stats(ctx, "ws"); err == nil {
			t.Fatal("failing stats should surface")
		}
		if _, err := eng.Consolidate(ctx, "ws", 0); err == nil {
			t.Fatal("failing consolidate should surface")
		}
	})

	t.Run("record event happy path", func(t *testing.T) {
		srv := cannedLadyM(t, nil, false, false, false, false, false)
		eng := remoteOver(t, srv.URL)
		if err := eng.RecordEvent(ctx, "ws", "a", "b", "c", "d"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRemoteDescriptionFallback(t *testing.T) {
	eng := &RemoteEngine{endpoint: "http://\x7f"}
	if got := eng.Description(); got != "ladym-remote" {
		t.Fatalf("fallback description = %q", got)
	}
}
