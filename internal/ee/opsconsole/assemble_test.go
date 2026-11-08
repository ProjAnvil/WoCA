package opsconsole

// Pins the shared console assembly (F4): every entry point that owns the
// console authority builds it through Assemble, so the ops paths must
// serve identically through each entry point's mount posture — the
// woca-ops facade's /api/ subtree and woca-scheduler's root mount — with
// the same auth posture in both.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/settings"
	"github.com/rs/zerolog"
)

// mountOpsFacade reproduces woca-ops's direct mount of the console: the
// API as the /api/ subtree (its SPA siblings are not ops paths).
func mountOpsFacade(asm *Assembly) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/", asm.Console.Handler())
	return mux
}

// mountScheduler reproduces woca-scheduler's management-listener mount:
// the console API at the root next to its /gw/ machine channel.
func mountScheduler(asm *Assembly) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", asm.Console.Handler())
	return mux
}

func newAssembly(t *testing.T) *Assembly {
	t.Helper()
	st, err := settings.Open("sqlite:" + filepath.Join(t.TempDir(), "ops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	asm, err := Assemble(st, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	return asm
}

func get(t *testing.T, srv *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestAssembleServesBothEntryPosturesIdentically: one shared assembly,
// each entry point's mount posture — every ops path answers with the same
// status and a byte-identical body, and the known route is pinned exactly.
func TestAssembleServesBothEntryPosturesIdentically(t *testing.T) {
	asm := newAssembly(t)
	if asm.Console == nil || asm.GW == nil || asm.Keys == nil || asm.Auth == nil {
		t.Fatalf("assembly incomplete: console=%v gw=%v keys=%v auth=%v", asm.Console, asm.GW, asm.Keys, asm.Auth)
	}
	facade := httptest.NewServer(mountOpsFacade(asm))
	t.Cleanup(facade.Close)
	sched := httptest.NewServer(mountScheduler(asm))
	t.Cleanup(sched.Close)

	for _, path := range []string{
		"/api/health",
		"/api/tenant/users",
		"/api/tenant/sessions",
		"/api/tenant/stats",
		"/api/gw/audit",
		"/api/skills",
	} {
		codeA, bodyA := get(t, facade, path)
		codeB, bodyB := get(t, sched, path)
		if codeA != codeB || bodyA != bodyB {
			t.Errorf("%s: facade = (%d, %q), scheduler = (%d, %q)", path, codeA, bodyA, codeB, bodyB)
		}
		if codeA != http.StatusOK {
			t.Errorf("%s: facade = %d, want 200 (open gate)", path, codeA)
		}
	}
	code, body := get(t, facade, "/api/health")
	if code != http.StatusOK || strings.TrimSpace(body) != `{"ok":true,"service":"woca-ops-console"}` {
		t.Fatalf("health via facade = %d %q", code, body)
	}
}

// TestAssembleGatesBothEntryPosturesIdentically: the auth posture is part
// of the assembly — with console credentials configured, both mounts gate
// every /api route behind the session cookie the same way.
func TestAssembleGatesBothEntryPosturesIdentically(t *testing.T) {
	t.Setenv("WOCA_CONSOLE_USER", "admin")
	t.Setenv("WOCA_CONSOLE_PASSWORD", "s3cret")
	asm := newAssembly(t)
	facade := httptest.NewServer(mountOpsFacade(asm))
	t.Cleanup(facade.Close)
	sched := httptest.NewServer(mountScheduler(asm))
	t.Cleanup(sched.Close)

	for name, srv := range map[string]*httptest.Server{"facade": facade, "scheduler": sched} {
		for _, path := range []string{"/api/tenant/users", "/api/skills", "/api/gw/audit"} {
			code, _ := get(t, srv, path)
			if code != http.StatusUnauthorized {
				t.Errorf("%s %s = %d, want 401 (gated)", name, path, code)
			}
		}
	}
}
