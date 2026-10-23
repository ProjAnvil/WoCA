package settings

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestManagedPinsSearchChain: the enterprise overlay delivers a default
// chain. The point of expressing enable/disable as a list is that a non-empty
// list pins the exact allowed set — a bool could not, because false is
// indistinguishable from "not managed".
func TestManagedPinsSearchChain(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// The user picked their own chain first.
	if err := st.Save(Settings{SearchProviders: []string{"tavily"}}); err != nil {
		t.Fatal(err)
	}

	st.SetManaged(Settings{
		SearchProviders:       []string{"searxng", "ddg-html"},
		SearchSearXNGEndpoint: "https://searx.internal.example.com",
	})

	got := st.Load()
	if len(got.SearchProviders) != 2 || got.SearchProviders[0] != "searxng" {
		t.Fatalf("SearchProviders = %v, want [searxng ddg-html]", got.SearchProviders)
	}
	if got.SearchSearXNGEndpoint != "https://searx.internal.example.com" {
		t.Fatalf("SearchSearXNGEndpoint = %q", got.SearchSearXNGEndpoint)
	}

	// Locked: a different set is rejected.
	if err := st.Save(Settings{SearchProviders: []string{"tavily"}}); !errors.Is(err, ErrManagedLocked) {
		t.Fatalf("changing a managed provider list should be locked, got %v", err)
	}
	if err := st.Save(Settings{SearchSearXNGEndpoint: "https://other.example"}); !errors.Is(err, ErrManagedLocked) {
		t.Fatalf("changing a managed endpoint should be locked, got %v", err)
	}

	// Re-saving the managed value is fine (it is what the UI sends back).
	if err := st.Save(Settings{SearchProviders: got.SearchProviders}); err != nil {
		t.Fatalf("re-saving the managed value: %v", err)
	}
}

// TestManagedSearchFieldsReportLocked: the frontend greys out exactly the
// keys the organization owns.
func TestManagedSearchFieldsReportLocked(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	st.SetManaged(Settings{
		SearchProviders:     []string{"searxng"},
		SearchDeniedDomains: []string{"spam.example"},
	})

	locked := map[string]bool{}
	for _, k := range st.ManagedFields() {
		locked[k] = true
	}
	for _, want := range []string{KeySearchProviders, KeySearchDeniedDomains} {
		if !locked[want] {
			t.Errorf("%s missing from ManagedFields: %v", want, st.ManagedFields())
		}
	}
	for _, unwanted := range []string{KeySearchSearXNGEndpoint, KeySearchCustomEndpoint, KeySearchAllowedDomains} {
		if locked[unwanted] {
			t.Errorf("%s should not be locked (not in the overlay)", unwanted)
		}
	}
}

// TestManagedSearchDomainLists: an empty list cannot be delivered, so an
// unrestricted allow-list is not expressible — only a pinned non-empty set.
// This documents the constraint rather than papering over it.
func TestManagedSearchDomainLists(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	st.SetManaged(Settings{SearchAllowedDomains: []string{"go.dev"}})
	got := st.Load()
	if len(got.SearchAllowedDomains) != 1 || got.SearchAllowedDomains[0] != "go.dev" {
		t.Fatalf("SearchAllowedDomains = %v", got.SearchAllowedDomains)
	}
	if err := st.Save(Settings{SearchAllowedDomains: []string{"evil.example"}}); !errors.Is(err, ErrManagedLocked) {
		t.Fatalf("changing a managed allow-list should be locked, got %v", err)
	}
}
