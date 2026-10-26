package edition

import (
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/settings"
)

// IdleWaiter surfaces the waiter edition.Plane embedded; nil, foreign and
// absent planes yield no waiter (the loop falls back to its poll cadence).
func TestIdleWaiter(t *testing.T) {
	if w := IdleWaiter(nil); w != nil {
		t.Fatal("IdleWaiter(nil) != nil")
	}

	// A plane this package did not build carries no embedded waiter.
	foreign := struct{ server.MsgPlane }{}
	if w := IdleWaiter(foreign.MsgPlane); w != nil {
		t.Fatalf("IdleWaiter(foreign) = %T, want nil", w)
	}

	st, err := settings.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	p := Plane(st, zerolog.Nop())
	defer p.Close()
	w := IdleWaiter(p)
	if w == nil {
		t.Fatal("IdleWaiter(edition plane) = nil, want the embedded waiter")
	}
}
