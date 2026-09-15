package pluginrt

import (
	"testing"

	"github.com/projanvil/woca/internal/runws"
)

// Plugin subprocesses (pdf.py) resolve relative paths against their cwd,
// which must follow the calling run's workspace in serve mode.
func TestPluginWorkdirFollowsRunWorkspace(t *testing.T) {
	fallback := "/srv/startup"
	ctx := runws.With(t.Context(), "/srv/session")
	if got := pluginWorkdir(ctx, fallback); got != "/srv/session" {
		t.Fatalf("pluginWorkdir(ws ctx) = %q, want /srv/session", got)
	}
	if got := pluginWorkdir(t.Context(), fallback); got != fallback {
		t.Fatalf("pluginWorkdir(plain ctx) = %q, want %q", got, fallback)
	}
}
