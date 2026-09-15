package skillsvc

// Gap tests: constructor validation, migration failure, and read misses.

import (
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/wocadb"
)

func TestNewStoreValidationAndMigrateFailure(t *testing.T) {
	if _, err := NewStore(nil); err == nil {
		t.Fatal("nil database should be rejected")
	}
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(db); err == nil {
		t.Fatal("migrate on a closed DB should fail NewStore")
	}
}

func TestReadMissesOnUnknownAsset(t *testing.T) {
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	if _, err := st.GetAsset(ctx, "ghost"); err == nil {
		t.Fatal("GetAsset on unknown id should fail")
	}
	if _, err := st.ListVersions(ctx, "ghost"); err != nil {
		t.Fatalf("ListVersions on unknown asset = %v, want empty", err)
	}
	if _, err := st.ResolveLabel(ctx, "ghost", "stable"); err == nil {
		t.Fatal("ResolveLabel on unknown asset should fail")
	}
	if labels, err := st.Labels(ctx, "ghost"); err != nil || len(labels) != 0 {
		t.Fatalf("Labels on unknown asset = %v %v", labels, err)
	}
}
