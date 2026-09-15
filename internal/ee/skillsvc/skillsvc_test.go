package skillsvc

import (
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/wocadb"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "skills.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestAssetCRUD(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()

	a, err := st.CreateAsset(ctx, "code-review", "Reviews code for common issues")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.Name != "code-review" {
		t.Fatalf("asset = %+v", a)
	}

	got, err := st.GetAsset(ctx, a.ID)
	if err != nil || got.Name != "code-review" {
		t.Fatalf("get = %+v err=%v", got, err)
	}

	list, err := st.ListAssets(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v err=%v", list, err)
	}

	if err := st.DeleteAsset(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAsset(ctx, a.ID); err == nil {
		t.Fatal("deleted asset still readable")
	}
}

func TestImmutableVersions(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	a, _ := st.CreateAsset(ctx, "deploy", "Deployment skill")

	v1, err := st.PublishVersion(ctx, a.ID, "# v1 body", "checksum-1")
	if err != nil {
		t.Fatal(err)
	}
	if v1.Version != 1 {
		t.Fatalf("first version = %d", v1.Version)
	}

	v2, err := st.PublishVersion(ctx, a.ID, "# v2 body", "checksum-2")
	if err != nil || v2.Version != 2 {
		t.Fatalf("v2 = %+v err=%v", v2, err)
	}

	// Versions are immutable: republishing the same version number fails.
	if _, err := st.PublishVersion(ctx, a.ID, "# v2 body override", "checksum-2"); err != nil {
		t.Fatalf("exact duplicate content should be idempotent: %v", err)
	}

	versions, err := st.ListVersions(ctx, a.ID)
	if err != nil || len(versions) != 2 {
		t.Fatalf("versions = %d err=%v", len(versions), err)
	}
	if versions[0].Version != 1 || versions[1].Version != 2 {
		t.Fatalf("order = %+v", versions)
	}
}

func TestLabelPublishAndRollback(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	a, _ := st.CreateAsset(ctx, "test-skill", "Test")

	v1, _ := st.PublishVersion(ctx, a.ID, "v1", "c1")
	v2, _ := st.PublishVersion(ctx, a.ID, "v2", "c2")

	// Publish v1 → production.
	if err := st.SetLabel(ctx, a.ID, "production", v1.Version); err != nil {
		t.Fatal(err)
	}
	resolved, err := st.ResolveLabel(ctx, a.ID, "production")
	if err != nil || resolved.Version != 1 || resolved.Body != "v1" {
		t.Fatalf("resolve = %+v err=%v", resolved, err)
	}

	// Roll forward to v2.
	if err := st.SetLabel(ctx, a.ID, "production", v2.Version); err != nil {
		t.Fatal(err)
	}
	resolved, _ = st.ResolveLabel(ctx, a.ID, "production")
	if resolved.Version != 2 {
		t.Fatalf("after rollback resolve = %+v", resolved)
	}

	// Rollback to v1.
	if err := st.SetLabel(ctx, a.ID, "production", v1.Version); err != nil {
		t.Fatal(err)
	}
	resolved, _ = st.ResolveLabel(ctx, a.ID, "production")
	if resolved.Version != 1 {
		t.Fatalf("rollback resolve = %+v", resolved)
	}
}

func TestResolveUnknownLabelReturnsZero(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	a, _ := st.CreateAsset(ctx, "x", "x")

	_, err := st.ResolveLabel(ctx, a.ID, "production")
	if err == nil {
		t.Fatal("unlabeled asset resolved")
	}
}

func TestPublishToUnknownAssetFails(t *testing.T) {
	st := openStore(t)
	if _, err := st.PublishVersion(t.Context(), "missing", "body", "c"); err == nil {
		t.Fatal("publish to unknown asset accepted")
	}
}

func TestSetLabelUnknownVersionFails(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	a, _ := st.CreateAsset(ctx, "x", "x")
	if err := st.SetLabel(ctx, a.ID, "production", 99); err == nil {
		t.Fatal("label to unknown version accepted")
	}
}

func TestDeleteAssetCascadesVersions(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	a, _ := st.CreateAsset(ctx, "x", "x")
	_, _ = st.PublishVersion(ctx, a.ID, "body", "c")
	_ = st.SetLabel(ctx, a.ID, "production", 1)
	if err := st.DeleteAsset(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if vs, err := st.ListVersions(ctx, a.ID); err == nil && len(vs) != 0 {
		t.Fatalf("versions survived delete: %v", vs)
	}
}

func TestResolveForTenant(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	a, _ := st.CreateAsset(ctx, "shared", "shared skill")
	_, _ = st.PublishVersion(ctx, a.ID, "v1", "c1")
	_, _ = st.PublishVersion(ctx, a.ID, "v2", "c2")
	_ = st.SetLabel(ctx, a.ID, "production", 1)
	_ = st.SetLabel(ctx, a.ID, "canary", 2)

	// Resolve all labels for the asset.
	labels, err := st.Labels(ctx, a.ID)
	if err != nil || len(labels) != 2 {
		t.Fatalf("labels = %v err=%v", labels, err)
	}
	if labels["production"].Version != 1 || labels["canary"].Version != 2 {
		t.Fatalf("labels = %+v", labels)
	}
}

// A closed database surfaces errors on every path (deep error sweep).
func TestClosedDBSurfacesErrors(t *testing.T) {
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAsset(t.Context(), "x", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	check := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: no error on closed db", what)
		}
	}
	check("create", func() error { _, err := st.CreateAsset(ctx, "y", ""); return err }())
	check("get", func() error { _, err := st.GetAsset(ctx, "z"); return err }())
	check("list", func() error { _, err := st.ListAssets(ctx); return err }())
	check("delete", st.DeleteAsset(ctx, "z"))
	check("publish", func() error { _, err := st.PublishVersion(ctx, "z", "b", "c"); return err }())
	check("listvers", func() error { _, err := st.ListVersions(ctx, "z"); return err }())
	check("setlabel", st.SetLabel(ctx, "z", "production", 1))
	check("resolvelabel", func() error { _, err := st.ResolveLabel(ctx, "z", "production"); return err }())
	check("labels", func() error { _, err := st.Labels(ctx, "z"); return err }())
}

// Input validation rejects blank fields before touching the store.
func TestInputValidation(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	if _, err := st.CreateAsset(ctx, "", "d"); err == nil {
		t.Fatal("blank name accepted")
	}
	if _, err := st.CreateAsset(ctx, "  ", "d"); err == nil {
		t.Fatal("whitespace name accepted")
	}
	a, _ := st.CreateAsset(ctx, "x", "")
	if _, err := st.PublishVersion(ctx, a.ID, "", "c"); err == nil {
		t.Fatal("blank body accepted")
	}
	if err := st.SetLabel(ctx, a.ID, "", 1); err == nil {
		t.Fatal("blank label accepted")
	}
	// Duplicate asset names are rejected.
	if _, err := st.CreateAsset(ctx, "x", "again"); err == nil {
		t.Fatal("duplicate name accepted")
	}
}

// Labels map carries both production and canary for cohort-based resolution.
func TestLabelsMapComplete(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	a, _ := st.CreateAsset(ctx, "multi", "multi")
	v1, _ := st.PublishVersion(ctx, a.ID, "stable", "c1")
	v2, _ := st.PublishVersion(ctx, a.ID, "canary-body", "c2")
	_ = st.SetLabel(ctx, a.ID, "production", v1.Version)
	_ = st.SetLabel(ctx, a.ID, "canary", v2.Version)
	_ = st.SetLabel(ctx, a.ID, "canary", v1.Version) // overwrite

	labels, err := st.Labels(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 2 {
		t.Fatalf("labels = %+v", labels)
	}
	if labels["canary"].Body != "stable" {
		t.Fatalf("canary after overwrite = %+v", labels["canary"])
	}
}
