package memory

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/platform/tenancy"
)

// TestRemoteEngineAgainstLiveLadyM drives the remote engine against a real
// ladyM enterprise deployment. Set WOCA_LADYM_E2E_URL (gateway endpoint,
// e.g. http://localhost:18080); optional WOCA_LADYM_E2E_USER/_PASS enable
// Basic auth (skip for auth=off deployments):
//
//	WOCA_LADYM_E2E_URL=http://localhost:18080 go test -run LiveLadyM . -v
func TestRemoteEngineAgainstLiveLadyM(t *testing.T) {
	endpoint := os.Getenv("WOCA_LADYM_E2E_URL")
	if endpoint == "" {
		t.Skip("WOCA_LADYM_E2E_URL not set")
	}
	eng, err := BuildRemote(RemoteConfig{
		Endpoint:  endpoint,
		Username:  os.Getenv("WOCA_LADYM_E2E_USER"),
		Password:  os.Getenv("WOCA_LADYM_E2E_PASS"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	ctx := tenancy.NewContext(context.Background(), tenancy.Context{
		TenantID: "t_e2e", UserID: "u_e2e", Scopes: []string{},
	})
	uniq := fmt.Sprintf("live probe %d", time.Now().UnixNano())

	id, err := eng.Remember(ctx, "/repo", uniq, []string{"e2e"}, "woca:e2e")
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if id == "" {
		t.Fatal("Remember returned no id (gate dropped a unique live probe)")
	}

	hits, err := eng.Recall(ctx, uniq, "/repo", 4)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("Recall found no hits for the just-remembered unique fact")
	}
	if hits[0].ID != id {
		t.Fatalf("Recall top hit = %q, want %q", hits[0].ID, id)
	}

	if err := eng.RecordEvent(ctx, "/repo", "woca-e2e", "probe", "live ladyM", "ok"); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}

	list, err := eng.List(ctx, "/repo", "", 5, 0)
	if err != nil || len(list) == 0 {
		t.Fatalf("List = %v err=%v", list, err)
	}
	st, err := eng.Stats(ctx, "/repo")
	if err != nil || st.Total == 0 {
		t.Fatalf("Stats = %+v err=%v", st, err)
	}
	if err := eng.Forget(ctx, id); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	t.Logf("live stats: %+v; description=%s", st, eng.Description())
}
