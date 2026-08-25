package nacosrt

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"path/filepath"

	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/settingswatch"
	"github.com/rs/zerolog"
)

func openTestStore(t *testing.T) *settings.Store {
	t.Helper()
	st, err := settings.Open("sqlite:" + filepath.Join(t.TempDir(), "nacosrt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// fakeNacos implements just enough of the v1 API surface to exercise the
// client and the runtime: config publish/get, the listener long-pull
// (shortened), and instance register/beat.
type fakeNacos struct {
	mu      sync.Mutex
	configs map[string]string // "group/dataId" → content

	registers int // successful instance registrations
	beats     int
	forgotten bool // Beat reports 20404 once to force re-register

	notify chan struct{} // closed+remade on every publish
}

func newFakeNacos() *fakeNacos {
	f := &fakeNacos{configs: map[string]string{}, notify: make(chan struct{})}
	return f
}

func (f *fakeNacos) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/nacos/v1/console/health/readiness", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/nacos/v1/cs/configs", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			key := r.FormValue("group") + "/" + r.FormValue("dataId")
			f.configs[key] = r.FormValue("content")
			close(f.notify)
			f.notify = make(chan struct{})
			fmt.Fprint(w, "true")
		case http.MethodGet:
			key := r.FormValue("group") + "/" + r.FormValue("dataId")
			if v, ok := f.configs[key]; ok {
				fmt.Fprint(w, v)
				return
			}
			http.Error(w, "config data not exist", http.StatusNotFound)
		}
	})
	mux.HandleFunc("/nacos/v1/cs/configs/listener", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		probe := r.FormValue("Listening-Configs")
		f.mu.Lock()
		changed := false
		for _, entry := range splitProbe(probe) {
			if content, ok := f.configs[entry.group+"/"+entry.dataID]; ok {
				if entry.md5 != contentMD5(content) {
					changed = true
				}
			}
		}
		notify := f.notify
		f.mu.Unlock()
		if changed {
			// Same wire shape as the real registry: percent-encoded entry.
			fmt.Fprint(w, url.QueryEscape(ConfigDataID+"\x02DEFAULT_GROUP")+"\x01")
			return
		}
		// Hang until the next publish or a short deadline.
		select {
		case <-notify:
			fmt.Fprint(w, url.QueryEscape(ConfigDataID+"\x02DEFAULT_GROUP")+"\x01")
		case <-r.Context().Done():
		case <-time.After(300 * time.Millisecond):
			fmt.Fprint(w, "")
		}
	})
	mux.HandleFunc("/nacos/v1/ns/instance", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodPost, http.MethodDelete:
			f.registers++
			fmt.Fprint(w, "ok")
		}
	})
	mux.HandleFunc("/nacos/v1/ns/instance/beat", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.forgotten {
			f.forgotten = false
			fmt.Fprint(w, `{"code":20404}`)
			return
		}
		f.beats++
		fmt.Fprint(w, `{"code":10200}`)
	})
	return mux
}

func splitProbe(probe string) []struct{ dataID, group, md5 string } {
	var out []struct{ dataID, group, md5 string }
	for _, entry := range splitSep(probe, "\x01") {
		parts := splitSep(entry, "\x02")
		if len(parts) >= 3 {
			out = append(out, struct{ dataID, group, md5 string }{parts[0], parts[1], parts[2]})
		}
	}
	return out
}

func splitSep(s, sep string) []string {
	var out []string
	start := 0
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			out = append(out, s[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func TestClientPublishGetRoundtrip(t *testing.T) {
	f := newFakeNacos()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := NewClient(srv.URL)

	if err := c.Ping(t.Context()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if _, err := c.Get(t.Context(), "DEFAULT_GROUP", "missing"); err != ErrNoConfig {
		t.Fatalf("missing entry: got %v", err)
	}
	if err := c.Publish(t.Context(), "DEFAULT_GROUP", "k", "v1"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	got, err := c.Get(t.Context(), "DEFAULT_GROUP", "k")
	if err != nil || got != "v1" {
		t.Fatalf("get after publish: %q %v", got, err)
	}
}

func TestClientListenDetectsChange(t *testing.T) {
	f := newFakeNacos()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := NewClient(srv.URL)

	_ = c.Publish(t.Context(), "DEFAULT_GROUP", ConfigDataID, "one")
	base := contentMD5("one")
	changed, err := c.Listen(t.Context(), "DEFAULT_GROUP", ConfigDataID, base, 50*time.Millisecond)
	if err != nil || changed {
		t.Fatalf("same-content listen should be quiet: %v %v", changed, err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = c.Publish(t.Context(), "DEFAULT_GROUP", ConfigDataID, "two")
	}()
	changed, err = c.Listen(t.Context(), "DEFAULT_GROUP", ConfigDataID, base, 2*time.Second)
	if err != nil || !changed {
		t.Fatalf("changed listen should fire: %v %v", changed, err)
	}
}

func TestRuntimeStartFailClosed(t *testing.T) {
	// Unreachable registry: Start must fail (the fail-closed contract).
	rt, err := New(Config{
		Server: "http://127.0.0.1:1", Service: "woca-worker", Advertise: "127.0.0.1:9091",
	}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(t.Context()); err == nil {
		t.Fatal("start against a dead registry must fail")
	}

	// Missing pack: also fatal — the role cannot align its configuration.
	f := newFakeNacos()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	st := openTestStore(t)
	rt2, err := New(Config{
		Server: srv.URL, Service: "woca-worker", Advertise: "127.0.0.1:9091",
		St: st,
	}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if err := rt2.Start(t.Context()); err == nil {
		t.Fatal("start without a delivered pack must fail")
	}
}

func TestRuntimeDeliversAndApplies(t *testing.T) {
	f := newFakeNacos()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := NewClient(srv.URL)

	st := openTestStore(t)
	applied := make(chan string, 4)
	rt, err := New(Config{
		Server: srv.URL, Service: "woca-worker", Advertise: "127.0.0.1:9091",
		St:       st,
		Identity: settingswatch.Identity{ServiceID: "worker-test", Role: "worker"},
		OnChange: func() { applied <- st.Load().Model },
	}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	// Bootstrap pack with a model setting.
	pack := &Pack{Version: packVersion, Generation: "g1", Settings: settings.Settings{Model: "glm-5"}}
	raw, _ := pack.Encode()
	if err := c.Publish(t.Context(), "DEFAULT_GROUP", ConfigDataID, raw); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Start applies the initial pack: the managed overlay carries the model.
	if got := st.Load().Model; got != "glm-5" {
		t.Fatalf("initial pack not applied: model %q", got)
	}
	if f.registers == 0 {
		t.Fatal("no registration happened")
	}

	// Drain the initial apply's OnChange signal before delivering the
	// second pack, so the wait below observes the hot apply only.
	select {
	case <-applied:
	default:
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go rt.Loop(ctx)

	// Deliver a new pack: the listener fires and the overlay hot-applies.
	pack2 := &Pack{Version: packVersion, Generation: "g2", Settings: settings.Settings{Model: "glm-6"}}
	raw2, _ := pack2.Encode()
	if err := c.Publish(t.Context(), "DEFAULT_GROUP", ConfigDataID, raw2); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-applied:
		if m != "glm-6" {
			t.Fatalf("hot-apply observed %q", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("delivered pack never applied")
	}
}

func TestPackVersionGuard(t *testing.T) {
	if _, err := DecodePack(`{"version":99,"generation":"","settings":{}}`); err == nil {
		t.Fatal("unknown pack version must be refused")
	}
	p, err := DecodePack(`{"version":1,"generation":"g","settings":{"model":"m"}}`)
	if err != nil || p.Settings.Model != "m" {
		t.Fatalf("decode: %v %+v", err, p)
	}
}
