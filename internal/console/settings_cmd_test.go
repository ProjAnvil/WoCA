package console

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/projanvil/woca/internal/settings"
)

// rebuildCounter records RebuildMemory/RebuildModel invocations; memErr makes
// the next memory rebuild fail.
type rebuildCounter struct {
	mu     sync.Mutex
	memErr error
	memory int
	model  int
}

func (r *rebuildCounter) rebuildMemory() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.memory++
	return r.memErr
}

func (r *rebuildCounter) rebuildModel() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.model++
	return nil
}

func (r *rebuildCounter) counts() (memory, model int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.memory, r.model
}

// newSettingsConsole wires a Console against a fresh settings store with the
// rebuild hooks stubbed by rc.
func newSettingsConsole(t *testing.T, memErr error) (*Console, *rebuildCounter) {
	t.Helper()
	st, err := settings.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rc := &rebuildCounter{memErr: memErr}
	return New(Deps{Settings: st, RebuildMemory: rc.rebuildMemory, RebuildModel: rc.rebuildModel}), rc
}

func TestSettingsSetMemoryKeyRouting(t *testing.T) {
	cases := []struct {
		name      string
		key, val  string
		wantOut   string
		wantMem   int // expected RebuildMemory calls
		wantModel int // expected RebuildModel calls
	}{
		{"engine", settings.KeyMemoryEngine, "ladym", "takes effect immediately", 1, 0},
		{"db path", settings.KeyLadyMDBPath, "/tmp/ladym.db", "takes effect immediately", 1, 0},
		{"embedding", settings.KeyLadyMEmbedding, "hashing", "takes effect immediately", 1, 0},
		{"embedding model", settings.KeyLadyMEmbeddingModel, "m-1", "takes effect immediately", 1, 0},
		{"embedding base url", settings.KeyLadyMEmbeddingBaseURL, "http://h:1", "takes effect immediately", 1, 0},
		{"llm", settings.KeyLadyMLLM, "none", "takes effect immediately", 1, 0},
		{"autoretain", settings.KeyLadyMAutoRetain, "true", "takes effect next run", 0, 0},
		{"topk", settings.KeyLadyMTopK, "8", "takes effect next run", 0, 0},
		{"system2", settings.KeyLadyMSystem2, "false", "takes effect next run", 0, 0},
		{"system2 interval", settings.KeyLadyMSystem2Interval, "120", "takes effect next run", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rc := newSettingsConsole(t, nil)
			res, err := c.Execute(context.Background(), "settings", []string{"set", tc.key, tc.val}, &Session{})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(res.Output, tc.key+" saved") {
				t.Fatalf("output = %q", res.Output)
			}
			if !strings.Contains(res.Output, tc.wantOut) {
				t.Fatalf("output = %q, want %q", res.Output, tc.wantOut)
			}
			mem, model := rc.counts()
			if mem != tc.wantMem || model != tc.wantModel {
				t.Fatalf("rebuilds memory=%d model=%d, want memory=%d model=%d", mem, model, tc.wantMem, tc.wantModel)
			}
		})
	}
}

func TestSettingsSetLadyMEmbeddingKeyRoutesToMemoryRebuild(t *testing.T) {
	c, rc := newSettingsConsole(t, nil)
	res, err := c.Execute(context.Background(), "settings", []string{"set", settings.KeyLadyMEmbeddingAPIKey, "sk-test"}, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "saved (encrypted); takes effect immediately") {
		t.Fatalf("output = %q", res.Output)
	}
	mem, model := rc.counts()
	if mem != 1 || model != 0 {
		t.Fatalf("rebuilds memory=%d model=%d, want memory=1 model=0", mem, model)
	}
}

func TestSettingsSetMemoryRebuildFailureSurfaces(t *testing.T) {
	c, rc := newSettingsConsole(t, errors.New("boom"))
	res, err := c.Execute(context.Background(), "settings", []string{"set", settings.KeyMemoryEngine, "ladym"}, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "memory engine rebuild failed") || !strings.Contains(res.Output, "boom") {
		t.Fatalf("output = %q", res.Output)
	}
	if mem, _ := rc.counts(); mem != 1 {
		t.Fatalf("RebuildMemory calls = %d, want 1", mem)
	}
}

func TestClampRunes(t *testing.T) {
	if got := clampRunes("short", 80); got != "short" {
		t.Fatalf("short = %q", got)
	}
	// ASCII cut at a rune boundary.
	if got := clampRunes(strings.Repeat("a", 100), 80); got != strings.Repeat("a", 80)+"…" {
		t.Fatalf("ascii = %q", got)
	}
	// Multi-byte runes: 100 CJK chars (300 bytes) cut at 80 bytes backs off
	// to the rune boundary at byte 78 (26 runes), none split.
	long := strings.Repeat("记", 100)
	if got, want := clampRunes(long, 80), strings.Repeat("记", 26)+"…"; got != want {
		t.Fatalf("cjk = %q, want %q", got, want)
	}
}
