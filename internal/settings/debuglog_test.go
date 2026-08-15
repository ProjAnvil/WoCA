package settings

import (
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/wocadb"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := wocadb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Store{db: db}
}

func TestDebugLogSettingRoundTrip(t *testing.T) {
	st := newTestStore(t)

	cfg := st.Load()
	if cfg.DebugLog {
		t.Fatal("debug-log must default to off")
	}
	if err := Apply(&cfg, KeyDebugLog, "on"); err != nil {
		t.Fatal(err)
	}
	if !cfg.DebugLog {
		t.Fatal("Apply(on) did not enable")
	}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}

	// Reload: the row survives (including an explicit later disable).
	if re := st.Load(); !re.DebugLog {
		t.Fatal("debug-log did not persist")
	}
	cfg = st.Load()
	if err := Apply(&cfg, KeyDebugLog, "false"); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if re := st.Load(); re.DebugLog {
		t.Fatal("explicit disable did not persist (empty-means-skip must not apply to bools)")
	}

	if err := Apply(&Settings{}, KeyDebugLog, "maybe"); err == nil {
		t.Fatal("invalid bool should be rejected")
	}
	if v := KeyValue(st.Load(), KeyDebugLog); v != "false" {
		t.Fatalf("KeyValue = %q", v)
	}
}
