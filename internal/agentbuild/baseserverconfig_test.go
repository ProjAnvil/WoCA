package agentbuild

import (
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/webgui"
	webdist "github.com/projanvil/woca/web"
)

// buildTestApp builds a fully assembled App against a fake provider, the same
// fixture shape the build tests use.
func buildTestApp(t *testing.T) *App {
	t.Helper()
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	ap, err := BuildAgent(t.Context(), baseOpts(t, url), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ap.PluginMgr.Close()
		ap.Agent.Close()
	})
	return ap
}

// tuiWebConfig assembles the TUI /web config exactly as cmd/woca's newConsole
// does: the shared base plus the embedded frontend.
func tuiWebConfig(ap *App) server.Config {
	cfg := ap.BaseServerConfig()
	cfg.Static = webdist.DistFS()
	return cfg
}

// TestTUIWebActivityAPI regression-tests the TUI /web drift (arch-cleanup
// 2026-11-08 F2): the embedded web UI the console's /web command starts must
// serve the session-activity surface — an in-process personal form — so an
// activity snapshot answers 200, never the 501 "not available in this
// deployment form" the hand-built config produced before the shared base.
func TestTUIWebActivityAPI(t *testing.T) {
	ap := buildTestApp(t)
	cfg := tuiWebConfig(ap)
	if !cfg.ActivityAPI {
		t.Fatal("TUI web config must enable the activity API (personal single-process form)")
	}

	m := webgui.New(cfg)
	url, err := m.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start web gui: %v", err)
	}
	defer m.Stop()

	// An unknown session yields the empty snapshot (200) — the same posture
	// as the personal serve form. 501 means the drift is back.
	resp, err := http.Get(url + "/api/sessions/no-such-session/activity")
	if err != nil {
		t.Fatalf("GET activity: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET activity = %d, want 200 (501 means the TUI web form lost the activity API)", resp.StatusCode)
	}
}

// TestBaseServerConfigFieldCoverage pins the base's field set from both
// directions: every field it sets belongs to the shared set, and every shared
// field (except the documented may-be-zero ones) is actually set — so a new
// field cannot silently join or leave the base. It consumes the canonical
// exported list (SharedBaseConfigFields), guarding that list itself against
// the base literal; Static is asserted absent separately below.
func TestBaseServerConfigFieldCoverage(t *testing.T) {
	ap := buildTestApp(t)
	cfg := ap.BaseServerConfig()
	baseConfigFields := SharedBaseConfigFields()

	if cfg.Static != nil {
		t.Fatal("base config must not carry a frontend (Static stays a per-form override)")
	}
	rt := reflect.TypeOf(cfg)
	rv := reflect.ValueOf(cfg)
	var set []string
	for i := range rt.NumField() {
		if !rv.Field(i).IsZero() {
			name := rt.Field(i).Name
			set = append(set, name)
			if !slices.Contains(baseConfigFields, name) {
				t.Errorf("base config sets %s, which is outside the shared base set", name)
			}
		}
	}
	mayBeZero := map[string]bool{"MemoryEngine": true, "SessionDir": true}
	for _, name := range baseConfigFields {
		if mayBeZero[name] {
			continue
		}
		if !slices.Contains(set, name) {
			t.Errorf("shared base field %s is missing from BaseServerConfig", name)
		}
	}
}
