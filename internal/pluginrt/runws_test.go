package pluginrt

import (
	"context"
	"testing"

	"github.com/projanvil/woca/internal/runws"
)

// Plugin subprocesses (pdf.py) resolve relative paths against their cwd,
// which must follow the calling run's workspace in serve mode.
func TestPluginWorkdirFollowsRunWorkspace(t *testing.T) {
	fallback := "/srv/startup"
	ctx := runws.With(context.Background(), "/srv/session")
	if got := pluginWorkdir(ctx, fallback); got != "/srv/session" {
		t.Fatalf("pluginWorkdir(ws ctx) = %q, want /srv/session", got)
	}
	if got := pluginWorkdir(context.Background(), fallback); got != fallback {
		t.Fatalf("pluginWorkdir(plain ctx) = %q, want %q", got, fallback)
	}
}
