package servefront

import (
	"reflect"
	"testing"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/server"
	webdist "github.com/projanvil/woca/web"
)

// TestServeConfigPersonalMatchesTUIBase pins the two web construction paths
// together (arch-cleanup 2026-11-08 F2): for the personal non-dispatch form,
// serveConfig must agree with the TUI /web config (the bare base) on every
// shared field, so the TUI's embedded web UI and `woca serve` cannot drift
// apart again. The shared field set is not re-declared here: it is consumed
// from agentbuild.SharedBaseConfigFields, the single canonical list next to
// BaseServerConfig, so a new base field is covered by this test the moment
// it joins the base. Func-typed fields cannot be compared for identity in
// Go; for those the test asserts both sides are wired (non-nil), which is
// the observable contract of the shared base.
func TestServeConfigPersonalMatchesTUIBase(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	ap, err := agentbuild.BuildAgent(t.Context(), baseOpts(t, url), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ap.PluginMgr.Close()
		ap.Agent.Close()
	})

	// The TUI /web form: the shared base itself (cmd/woca only adds Static).
	tui := ap.BaseServerConfig()

	// The personal non-dispatch serve form, assembled the way the woca
	// binary's serve command does: the personal edition (embedded static,
	// local trust), no auth plane, no dispatch, default FS and session dir.
	got := serveConfig(ap, serveForm{
		ed:             Edition{Static: webdist.DistFS()},
		sessionDir:     serveSessionDir(),
		fsSvc:          defaultFS("127.0.0.1:3484"),
		grafanaURL:     "",
		sandboxFlagged: false,
	})

	if !got.ActivityAPI {
		t.Fatal("personal serve form must keep ActivityAPI on (no dispatch)")
	}

	for _, name := range agentbuild.SharedBaseConfigFields() {
		f, ok := reflect.TypeOf(server.Config{}).FieldByName(name)
		if !ok {
			t.Fatalf("server.Config has no field %s", name)
		}
		a := reflect.ValueOf(tui).FieldByName(name)
		b := reflect.ValueOf(got).FieldByName(name)
		if f.Type.Kind() == reflect.Func {
			if a.IsNil() || b.IsNil() {
				t.Fatalf("shared func field %s must be wired on both forms (tui nil: %v, serve nil: %v)", name, a.IsNil(), b.IsNil())
			}
			continue
		}
		if !reflect.DeepEqual(a.Interface(), b.Interface()) {
			t.Fatalf("shared field %s drifted between the TUI and serve forms: tui=%v serve=%v", name, a, b)
		}
	}
}
