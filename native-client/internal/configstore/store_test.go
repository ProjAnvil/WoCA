package configstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorePersistsActiveProfileAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.SaveProfile(t.Context(), "https://Enterprise.Example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL != "https://enterprise.example.com" {
		t.Fatalf("BaseURL = %q", got.BaseURL)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	active, ok, err := s.ActiveProfile(t.Context())
	if err != nil || !ok {
		t.Fatalf("ActiveProfile = %#v, %v, %v", active, ok, err)
	}
	if active.ID != got.ID || active.BaseURL != got.BaseURL {
		t.Fatalf("active = %#v, want %#v", active, got)
	}
}

func TestStorePersistsPreference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := s.PutPreferenceVersioned(t.Context(), "theme", "dark", 1)
	if err != nil || !applied {
		t.Fatalf("PutPreferenceVersioned() applied = %v, err = %v", applied, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	value, ok, err := s.Preference(t.Context(), "theme")
	if err != nil || !ok || value != "dark" {
		t.Fatalf("Preference() = %q, %v, %v", value, ok, err)
	}
}

func TestOpenAndPreferenceReadsDoNotClaimPreferenceEpoch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := first.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("schema version = %d, want 2", version)
	}
	if epoch := storedPreferenceEpoch(t, first); epoch != 0 {
		t.Fatalf("epoch after first open = %d, want unclaimed 0", epoch)
	}
	if value, found, err := first.Preference(t.Context(), "woca-theme"); err != nil || found || value != "" {
		t.Fatalf("missing Preference() = %q, %v, %v", value, found, err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	defer second.Close()
	if value, found, err := second.Preference(t.Context(), "woca.pref.model"); err != nil || found || value != "" {
		t.Fatalf("second missing Preference() = %q, %v, %v", value, found, err)
	}
	if epoch := storedPreferenceEpoch(t, second); epoch != 0 {
		t.Fatalf("epoch after second open/read = %d, want unclaimed 0", epoch)
	}
}

func TestReadOnlyExtraStoreDoesNotFenceCurrentPreferenceWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	storeA, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer storeA.Close()
	applied, err := storeA.PutPreferenceVersioned(t.Context(), "woca.pref.model", "a-one", 1)
	if err != nil || !applied {
		t.Fatalf("store A revision 1 applied = %v, err = %v", applied, err)
	}
	claimedEpoch := storedPreferenceEpoch(t, storeA)

	for launch := 1; launch <= 3; launch++ {
		readOnlyStore, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if value, found, err := readOnlyStore.Preference(t.Context(), "woca.pref.model"); err != nil || !found || value != "a-one" {
			t.Fatalf("read-only Store %d Preference() = %q, %v, %v; want a-one", launch, value, found, err)
		}
		if err := readOnlyStore.Close(); err != nil {
			t.Fatal(err)
		}
		if epoch := storedPreferenceEpoch(t, storeA); epoch != claimedEpoch {
			t.Fatalf("read-only Store %d changed epoch from %d to %d", launch, claimedEpoch, epoch)
		}
	}

	applied, err = storeA.PutPreferenceVersioned(t.Context(), "woca.pref.model", "a-two", 2)
	if err != nil || !applied {
		t.Fatalf("store A revision 2 after read-only launch applied = %v, err = %v", applied, err)
	}
}

func TestConcurrentFirstWritesClaimOneEpoch(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	type outcome struct {
		applied bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for key, value := range map[string]string{
		"woca-theme":       "dark",
		"woca.pref.locale": "en",
	} {
		go func() {
			<-start
			applied, err := store.PutPreferenceVersioned(t.Context(), key, value, 1)
			results <- outcome{applied: applied, err: err}
		}()
	}
	close(start)
	for range 2 {
		result := <-results
		if result.err != nil || !result.applied {
			t.Fatalf("concurrent first write applied = %v, err = %v", result.applied, result.err)
		}
	}
	if epoch := storedPreferenceEpoch(t, store); epoch != 1 {
		t.Fatalf("epoch after concurrent first writes = %d, want exactly 1", epoch)
	}
}

func TestPreferenceWriteRetriesAfterPostClaimFailureWithoutNewEpoch(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.Exec(`
		CREATE TRIGGER reject_test_preference
		BEFORE INSERT ON preferences
		BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`); err != nil {
		t.Fatal(err)
	}

	applied, err := store.PutPreferenceVersioned(t.Context(), "woca-theme", "dark", 1)
	if err == nil || applied {
		t.Fatalf("injected write applied = %v, err = %v; want failure", applied, err)
	}
	claimedEpoch := storedPreferenceEpoch(t, store)
	if claimedEpoch != 1 {
		t.Fatalf("epoch after failed post-claim write = %d, want 1", claimedEpoch)
	}
	if _, err := store.db.Exec(`DROP TRIGGER reject_test_preference`); err != nil {
		t.Fatal(err)
	}
	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	applied, err = store.PutPreferenceVersioned(canceledCtx, "woca-theme", "dark", 1)
	if err == nil || applied {
		t.Fatalf("canceled retry applied = %v, err = %v; want failure", applied, err)
	}
	if epoch := storedPreferenceEpoch(t, store); epoch != claimedEpoch {
		t.Fatalf("canceled retry changed epoch from %d to %d", claimedEpoch, epoch)
	}

	applied, err = store.PutPreferenceVersioned(t.Context(), "woca-theme", "dark", 1)
	if err != nil || !applied {
		t.Fatalf("retry applied = %v, err = %v", applied, err)
	}
	if epoch := storedPreferenceEpoch(t, store); epoch != claimedEpoch {
		t.Fatalf("retry changed epoch from %d to %d", claimedEpoch, epoch)
	}
}

func TestPreferenceEpochOverflowFailsClosed(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.Exec(`UPDATE preference_epoch SET epoch = ? WHERE singleton = 1`, int64(9_223_372_036_854_775_807)); err != nil {
		t.Fatal(err)
	}

	applied, err := store.PutPreferenceVersioned(t.Context(), "woca-theme", "dark", 1)
	if err == nil || applied {
		t.Fatalf("overflow write applied = %v, err = %v; want fail-closed error", applied, err)
	}
	if _, found, err := store.Preference(t.Context(), "woca-theme"); err != nil || found {
		t.Fatalf("Preference() found = %v, err = %v after overflow", found, err)
	}
}

func TestOpenMigratesVersionOnePreferencesWithoutLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	prepareDatabase(t, path, validVersionOneSchema+`
		INSERT INTO preferences (key, value, updated_at)
		VALUES ('woca-theme', 'dark', 123);
	`)

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	value, found, err := store.Preference(t.Context(), "woca-theme")
	if err != nil || !found || value != "dark" {
		t.Fatalf("Preference() = %q, %v, %v; want migrated dark", value, found, err)
	}
	var version int
	if err := store.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("schema version = %d, want 2", version)
	}
	if !tableExists(t, path, "preference_versions") {
		t.Fatal("version-1 migration did not create preference_versions")
	}
	if epoch := storedPreferenceEpoch(t, store); epoch != 0 {
		t.Fatalf("version-1 migration claimed epoch %d without a write", epoch)
	}
}

func TestVersionedPreferenceWritesFenceOlderStoresAndOrderRevisions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	storeA, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := storeA.PutPreferenceVersioned(t.Context(), "woca-theme", "dark", 1)
	if err != nil || !applied {
		t.Fatalf("store A initial write applied = %v, err = %v", applied, err)
	}

	type writeOutcome struct {
		applied bool
		err     error
	}
	releaseA := make(chan struct{})
	aDone := make(chan writeOutcome, 1)
	go func() {
		<-releaseA
		applied, err := storeA.PutPreferenceVersioned(
			t.Context(), "woca.pref.model", "stale-a", 9_000,
		)
		aDone <- writeOutcome{applied: applied, err: err}
	}()

	storeB, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if storeB.epoch != 0 {
		t.Fatalf("store B epoch after open = %d, want unclaimed 0", storeB.epoch)
	}
	applied, err = storeA.PutPreferenceVersioned(t.Context(), "woca-theme", "light", 2)
	if err != nil || !applied {
		t.Fatalf("store A write after B open applied = %v, err = %v", applied, err)
	}
	applied, err = storeB.PutPreferenceVersioned(t.Context(), "woca.pref.model", "b-one", 1)
	if err != nil || !applied {
		t.Fatalf("store B revision 1 applied = %v, err = %v", applied, err)
	}
	if storeB.epoch <= storeA.epoch {
		t.Fatalf("store B epoch = %d, want greater than store A epoch %d after B write", storeB.epoch, storeA.epoch)
	}
	fenced, err := storeA.PutPreferenceVersioned(t.Context(), "woca-theme", "auto", 8_000)
	if err != nil || fenced {
		t.Fatalf("store A write after B write applied = %v, err = %v; want epoch fence", fenced, err)
	}
	close(releaseA)
	staleA := <-aDone
	if staleA.err != nil || staleA.applied {
		t.Fatalf("delayed store A applied = %v, err = %v; want stale no-op", staleA.applied, staleA.err)
	}
	value, found, err := storeB.Preference(t.Context(), "woca.pref.model")
	if err != nil || !found || value != "b-one" {
		t.Fatalf("Preference() = %q, %v, %v; want b-one", value, found, err)
	}

	applied, err = storeB.PutPreferenceVersioned(t.Context(), "woca.pref.model", "b-equal", 1)
	if err != nil || applied {
		t.Fatalf("equal store B revision applied = %v, err = %v; want stale no-op", applied, err)
	}
	applied, err = storeB.PutPreferenceVersioned(t.Context(), "woca.pref.model", "b-two", 2)
	if err != nil || !applied {
		t.Fatalf("store B revision 2 applied = %v, err = %v", applied, err)
	}
	applied, err = storeB.PutPreferenceVersioned(t.Context(), "woca.pref.model", "b-stale", 1)
	if err != nil || applied {
		t.Fatalf("older store B revision applied = %v, err = %v; want stale no-op", applied, err)
	}

	startConcurrent := make(chan struct{})
	concurrentResults := make(chan struct {
		revision uint64
		writeOutcome
	}, 2)
	for revision, value := range map[uint64]string{3: "b-three", 4: "b-four"} {
		go func() {
			<-startConcurrent
			applied, err := storeB.PutPreferenceVersioned(t.Context(), "woca.pref.model", value, revision)
			concurrentResults <- struct {
				revision uint64
				writeOutcome
			}{revision: revision, writeOutcome: writeOutcome{applied: applied, err: err}}
		}()
	}
	close(startConcurrent)
	for range 2 {
		result := <-concurrentResults
		if result.err != nil {
			t.Fatalf("concurrent store B revision %d error = %v", result.revision, result.err)
		}
		if result.revision == 4 && !result.applied {
			t.Fatal("highest concurrent store B revision was not applied")
		}
	}
	value, found, err = storeB.Preference(t.Context(), "woca.pref.model")
	if err != nil || !found || value != "b-four" {
		t.Fatalf("Preference() after concurrent writes = %q, %v, %v; want b-four", value, found, err)
	}
	if err := storeA.Close(); err != nil {
		t.Fatal(err)
	}
	if err := storeB.Close(); err != nil {
		t.Fatal(err)
	}

	storeC, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer storeC.Close()
	applied, err = storeC.PutPreferenceVersioned(t.Context(), "woca.pref.model", "c-one", 1)
	if err != nil || !applied {
		t.Fatalf("store C revision 1 applied = %v, err = %v", applied, err)
	}
	if storeC.epoch <= storeB.epoch {
		t.Fatalf("store C epoch = %d, want greater than store B epoch %d after C write", storeC.epoch, storeB.epoch)
	}
	value, found, err = storeC.Preference(t.Context(), "woca.pref.model")
	if err != nil || !found || value != "c-one" {
		t.Fatalf("Preference() = %q, %v, %v; want c-one", value, found, err)
	}
}

func TestStoreRejectsURLWithCredentialsQueryOrFragment(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, rawURL := range []string{
		"https://u:p@example.com",
		"https://example.com?q=1",
		"https://example.com/#x",
		"https://example.com/app",
	} {
		t.Run(rawURL, func(t *testing.T) {
			if _, err := s.SaveProfile(t.Context(), rawURL); err == nil {
				t.Fatalf("SaveProfile(%q) error = nil", rawURL)
			}
		})
	}

	_, ok, err := s.ActiveProfile(t.Context())
	if err != nil || ok {
		t.Fatalf("ActiveProfile() ok = %v, err = %v", ok, err)
	}
}

func TestStoreAllowsHTTPOnlyForLoopback(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, rawURL := range []string{
		"http://localhost:8080",
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
	} {
		t.Run(rawURL, func(t *testing.T) {
			profile, err := s.SaveProfile(t.Context(), rawURL)
			if err != nil {
				t.Fatal(err)
			}
			if profile.BaseURL != rawURL {
				t.Fatalf("BaseURL = %q, want %q", profile.BaseURL, rawURL)
			}
		})
	}

	if _, err := s.SaveProfile(t.Context(), "http://example.com"); err == nil {
		t.Fatal("SaveProfile(remote HTTP) error = nil")
	}
}

func TestNormalizeBaseURLAllowsOnlyExactHTTPLoopbackHosts(t *testing.T) {
	for rawURL, want := range map[string]string{
		"http://LOCALHOST:8080": "http://localhost:8080",
		"http://127.0.0.1:8080": "http://127.0.0.1:8080",
		"http://[::1]:8080":     "http://[::1]:8080",
	} {
		got, err := NormalizeBaseURL(rawURL)
		if err != nil || got != want {
			t.Fatalf("NormalizeBaseURL(%q) = %q, %v; want %q, nil", rawURL, got, err, want)
		}
	}

	for _, rawURL := range []string{
		"http://127.0.0.2:8080",
		"http://127.255.255.255:8080",
		"http://[::2]:8080",
	} {
		if _, err := NormalizeBaseURL(rawURL); err == nil {
			t.Fatalf("NormalizeBaseURL(%q) error = nil", rawURL)
		}
	}
}

func TestNormalizeBaseURLCanonicalizesAuthorityAndPorts(t *testing.T) {
	for rawURL, want := range map[string]string{
		"https://EXAMPLE.COM:443/":       "https://example.com",
		"https://EXAMPLE.COM:8443/":      "https://example.com:8443",
		"http://LOCALHOST:80/":           "http://localhost",
		"http://127.0.0.1:8080/":         "http://127.0.0.1:8080",
		"http://[0:0:0:0:0:0:0:1]:8080/": "http://[::1]:8080",
		"http://[::1]:80/":               "http://[::1]",
	} {
		got, err := NormalizeBaseURL(rawURL)
		if err != nil || got != want {
			t.Fatalf("NormalizeBaseURL(%q) = %q, %v; want %q, nil", rawURL, got, err, want)
		}
	}

	for _, rawURL := range []string{
		"https://example.com:",
		"https://example.com:0",
		"https://example.com:65536",
		"https://example.com:not-a-port",
		"http://[localhost]:8080",
		"http://[127.0.0.1]:8080",
		"http://[fe80::1%25en0]:8080",
		"http://[fe80::1%en0]:8080",
	} {
		if _, err := NormalizeBaseURL(rawURL); err == nil {
			t.Fatalf("NormalizeBaseURL(%q) error = nil", rawURL)
		}
	}
}

func TestNormalizeBaseURLRejectsDefaultIgnorableUnicode(t *testing.T) {
	for _, rawURL := range []string{
		"https://exa\u00admple.com",
		"https://exa\u200bmple.com",
		"https://exa\ufeffmple.com",
		"https://exa\U000e0100mple.com",
		"https://exa\U000e0001mple.com",
		"https://exa%E2%80%8Bmple.com",
	} {
		if _, err := NormalizeBaseURL(rawURL); err == nil {
			t.Fatalf("NormalizeBaseURL(%q) error = nil", rawURL)
		}
	}

	got, err := NormalizeBaseURL("https://\u4f8b\u5b50.\u6d4b\u8bd5")
	if err != nil || got != "https://xn--fsqu00a.xn--0zwm56d" {
		t.Fatalf("NormalizeBaseURL(visible IDN) = %q, %v", got, err)
	}
}

func TestNormalizeBaseURLRejectsAlternateIPv4Forms(t *testing.T) {
	for _, rawURL := range []string{
		"http://127.1",
		"https://127.1",
		"https://2130706433",
		"https://0x7f000001",
		"https://0177.0.0.1",
		"https://127.0.0.1.",
		"https://4294967296",
		"https://09.0.0.1",
		"https://１２７.０.０.１",
		"https://127。0。0。1",
	} {
		if _, err := NormalizeBaseURL(rawURL); err == nil {
			t.Fatalf("NormalizeBaseURL(%q) error = nil", rawURL)
		}
	}

	for rawURL, want := range map[string]string{
		"https://127.0.0.1":   "https://127.0.0.1",
		"https://123.example": "https://123.example",
	} {
		got, err := NormalizeBaseURL(rawURL)
		if err != nil || got != want {
			t.Fatalf("NormalizeBaseURL(%q) = %q, %v; want %q, nil", rawURL, got, err, want)
		}
	}
}

func TestNormalizeBaseURLRejectsRawPercentInAuthority(t *testing.T) {
	for _, rawURL := range []string{
		"https://exa%E4%BE%8Bmple.com",
		"https://xn--fsqu00a.xn--0zwm56d%2e",
	} {
		if _, err := NormalizeBaseURL(rawURL); err == nil {
			t.Fatalf("NormalizeBaseURL(%q) error = nil", rawURL)
		}
	}
}

func TestOpenRejectsUnversionedMixedCaseApplicationObjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	prepareDatabase(t, path, `CREATE TABLE CONNECTION_PROFILES (id INTEGER PRIMARY KEY)`)

	if store, err := Open(path); err == nil {
		store.Close()
		t.Fatal("Open() error = nil for unversioned mixed-case application object")
	}
	if tableExists(t, path, "schema_migrations") {
		t.Fatal("Open() adopted a schema version for unversioned application objects")
	}
	if countApplicationObjects(t, path) != 1 {
		t.Fatal("Open() changed unversioned application objects")
	}
}

func TestOpenRejectsFutureSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	prepareDatabase(t, path, `
		CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
		INSERT INTO schema_migrations (version, applied_at) VALUES (3, 0);
	`)

	if store, err := Open(path); err == nil {
		store.Close()
		t.Fatal("Open() error = nil for future schema version")
	}
	if countApplicationObjects(t, path) != 0 {
		t.Fatal("Open() created application objects for future schema")
	}
}

func TestOpenRejectsIncompatibleVersionTwoSchemaWithoutRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	prepareDatabase(t, path, invalidVersionTwoSchemaWithoutPreferenceVersions)

	store, err := Open(path)
	if err == nil {
		store.Close()
		t.Fatal("Open() error = nil for incomplete version-2 schema")
	}
	if !strings.Contains(err.Error(), "missing table preference_versions") {
		t.Fatalf("Open() error = %q, want missing preference_versions", err)
	}
	if tableExists(t, path, "preference_versions") {
		t.Fatal("Open() silently repaired the incomplete version-2 schema")
	}
}

func TestOpenRejectsAlteredVersionTwoOrderingSchema(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{"epoch singleton check", func(schema string) string {
			return strings.Replace(schema, "CHECK (singleton = 1)", "CHECK (singleton > 0)", 1)
		}},
		{"revision positive check", func(schema string) string {
			return strings.Replace(schema, "CHECK (revision > 0)", "CHECK (revision >= 0)", 1)
		}},
		{"version foreign key", func(schema string) string {
			return strings.Replace(schema, "ON DELETE CASCADE,\n    epoch", "ON DELETE RESTRICT,\n    epoch", 1)
		}},
		{"missing epoch singleton", func(schema string) string {
			return strings.Replace(schema, "INSERT INTO preference_epoch (singleton, epoch) VALUES (1, 0);", "", 1)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "client.db")
			prepareDatabase(t, path, tt.mutate(validVersionTwoSchema))
			if store, err := Open(path); err == nil {
				store.Close()
				t.Fatal("Open() error = nil for altered version-2 ordering schema")
			}
		})
	}
}

func TestOpenRejectsIncompatibleVersionOneSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	prepareDatabase(t, path, `
		CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
		INSERT INTO schema_migrations (version, applied_at) VALUES (1, 0);
		CREATE TABLE connection_profiles (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			base_url TEXT NOT NULL UNIQUE,
			active INTEGER NOT NULL DEFAULT 0 CHECK (active IN (0, 1)),
			created_at INTEGER NOT NULL
		);
		CREATE UNIQUE INDEX one_active_profile ON connection_profiles(active) WHERE active = 1;
	`)

	if store, err := Open(path); err == nil {
		store.Close()
		t.Fatal("Open() error = nil for incompatible version-1 schema")
	}
}

func TestOpenDoesNotRepairVersionOneSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	prepareDatabase(t, path, strings.Replace(validVersionOneSchema, `CREATE TABLE preferences (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
`, "", 1))

	if store, err := Open(path); err == nil {
		store.Close()
		t.Fatal("Open() error = nil for incomplete version-1 schema")
	}
	if tableExists(t, path, "preferences") {
		t.Fatal("Open() repaired the incomplete version-1 schema")
	}
}

func TestOpenRejectsAlteredVersionOneSchemaSemantics(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{"base URL unique", func(schema string) string {
			return strings.Replace(schema, "base_url TEXT NOT NULL UNIQUE", "base_url TEXT NOT NULL", 1)
		}},
		{"active default", func(schema string) string {
			return strings.Replace(schema, "DEFAULT 0", "DEFAULT 1", 1)
		}},
		{"active check", func(schema string) string {
			return strings.Replace(schema, "CHECK (active IN (0, 1))", "CHECK (active IN (0, 2))", 1)
		}},
		{"profile autoincrement", func(schema string) string {
			return strings.Replace(schema, "INTEGER PRIMARY KEY AUTOINCREMENT", "INTEGER PRIMARY KEY", 1)
		}},
		{"native session foreign key", func(schema string) string {
			return strings.Replace(schema, "ON DELETE CASCADE", "ON DELETE RESTRICT", 1)
		}},
		{"active predicate", func(schema string) string {
			return strings.Replace(schema, "WHERE active = 1", "WHERE active IN (0, 1)", 1)
		}},
		{"comment spoof", func(schema string) string {
			return strings.Replace(schema, "DEFAULT 0 CHECK", "DEFAULT 0 /* spoof */ CHECK", 1)
		}},
		{"foreign key update", func(schema string) string {
			return strings.Replace(schema, "ON DELETE CASCADE", "ON UPDATE CASCADE ON DELETE CASCADE", 1)
		}},
		{"extra foreign key", func(schema string) string {
			return strings.Replace(schema, "last_validated_at INTEGER NOT NULL\n);", "last_validated_at INTEGER NOT NULL,\n    FOREIGN KEY (device_id) REFERENCES connection_profiles(id)\n);", 1)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "client.db")
			prepareDatabase(t, path, tt.mutate(validVersionOneSchema))
			if store, err := Open(path); err == nil {
				store.Close()
				t.Fatal("Open() error = nil for altered version-1 schema")
			}
		})
	}
}

func prepareDatabase(t *testing.T, path, statements string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(statements); err != nil {
		t.Fatal(err)
	}
}

func storedPreferenceEpoch(t *testing.T, store *Store) int64 {
	t.Helper()
	var epoch int64
	if err := store.db.QueryRow(`SELECT epoch FROM preference_epoch WHERE singleton = 1`).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	return epoch
}

func countApplicationObjects(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE lower(name) IN ('connection_profiles', 'preferences', 'native_sessions', 'one_active_profile', 'preference_epoch', 'preference_versions')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func tableExists(t *testing.T, path, name string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count == 1
}

const validVersionOneSchema = `
CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
INSERT INTO schema_migrations (version, applied_at) VALUES (1, 0);
CREATE TABLE connection_profiles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    base_url TEXT NOT NULL UNIQUE,
    active INTEGER NOT NULL DEFAULT 0 CHECK (active IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX one_active_profile ON connection_profiles(active) WHERE active = 1;
CREATE TABLE preferences (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE TABLE native_sessions (
    profile_id INTEGER PRIMARY KEY REFERENCES connection_profiles(id) ON DELETE CASCADE,
    device_id TEXT NOT NULL,
    session_ciphertext BLOB NOT NULL,
    expires_at INTEGER NOT NULL,
    last_validated_at INTEGER NOT NULL
);
`

const invalidVersionTwoSchemaWithoutPreferenceVersions = `
CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
INSERT INTO schema_migrations (version, applied_at) VALUES (2, 0);
CREATE TABLE connection_profiles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    base_url TEXT NOT NULL UNIQUE,
    active INTEGER NOT NULL DEFAULT 0 CHECK (active IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX one_active_profile ON connection_profiles(active) WHERE active = 1;
CREATE TABLE preferences (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE TABLE native_sessions (
    profile_id INTEGER PRIMARY KEY REFERENCES connection_profiles(id) ON DELETE CASCADE,
    device_id TEXT NOT NULL,
    session_ciphertext BLOB NOT NULL,
    expires_at INTEGER NOT NULL,
    last_validated_at INTEGER NOT NULL
);
CREATE TABLE preference_epoch (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    epoch INTEGER NOT NULL CHECK (epoch >= 0)
);
INSERT INTO preference_epoch (singleton, epoch) VALUES (1, 0);
`

const validVersionTwoSchema = invalidVersionTwoSchemaWithoutPreferenceVersions + `
CREATE TABLE preference_versions (
    key TEXT PRIMARY KEY REFERENCES preferences(key) ON DELETE CASCADE,
    epoch INTEGER NOT NULL CHECK (epoch > 0),
    revision INTEGER NOT NULL CHECK (revision > 0)
);
`

func TestStoreSchemaHasNoAPIKeyOrOAuthTokenColumns(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rows, err := s.db.QueryContext(t.Context(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		columns, err := s.db.QueryContext(t.Context(), `SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		for columns.Next() {
			var column string
			if err := columns.Scan(&column); err != nil {
				columns.Close()
				t.Fatal(err)
			}
			normalized := strings.ToLower(strings.ReplaceAll(column, "-", "_"))
			for _, forbidden := range []string{"apikey", "api_key", "access_token", "refresh_token"} {
				if strings.Contains(normalized, forbidden) {
					columns.Close()
					t.Fatalf("%s.%s must not store %s", table, column, forbidden)
				}
			}
		}
		if err := columns.Err(); err != nil {
			columns.Close()
			t.Fatal(err)
		}
		columns.Close()
	}
}

// ProfileBridge round-trip (v1.6 entry ②): the per-profile target bridge
// survives normalization and never bleeds between profiles.
func TestProfileBridgeRoundTrip(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.SaveProfile(ctx, "https://edge.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveProfile(ctx, "https://other.example.com"); err != nil {
		t.Fatal(err)
	}

	if got := s.ProfileBridge(ctx, "https://edge.example.com"); got != "" {
		t.Fatalf("unset by default, got %q", got)
	}
	if err := s.SetProfileBridge(ctx, "https://Edge.Example.com:443/", "br_target"); err != nil {
		t.Fatal(err)
	}
	if got := s.ProfileBridge(ctx, "https://edge.example.com"); got != "br_target" {
		t.Fatalf("normalized lookup = %q", got)
	}
	if got := s.ProfileBridge(ctx, "https://other.example.com"); got != "" {
		t.Fatalf("no bleed across profiles, got %q", got)
	}

	if err := s.SetProfileBridge(ctx, "https://edge.example.com", ""); err != nil {
		t.Fatal(err)
	}
	if got := s.ProfileBridge(ctx, "https://edge.example.com"); got != "" {
		t.Fatalf("cleared = %q", got)
	}
	if err := s.SetProfileBridge(ctx, "https://edge.example.com", strings.Repeat("b", 129)); err == nil {
		t.Fatal("oversized id accepted")
	}
}
