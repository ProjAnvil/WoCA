package relay

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGenerationClaimConflictAndExpiry(t *testing.T) {
	now := time.Now()
	r := NewGenerationRegistry()
	r.now = func() time.Time { return now }

	adopted, evicted, err := r.Claim(7, "gen-a")
	if err != nil || !adopted || evicted {
		t.Fatalf("first claim: adopted=%v evicted=%v err=%v", adopted, evicted, err)
	}
	// A competing generation loses while gen-a is live (409, design §6.2).
	if _, _, err := r.Claim(7, "gen-b"); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("competing claim: %v, want ErrGenerationConflict", err)
	}
	if !r.Online(7) || !r.Current(7, "gen-a") || r.Current(7, "gen-b") {
		t.Fatal("registry state wrong after conflict")
	}
	// The lease expires: gen-b takes over, gen-a is a zombie.
	now = now.Add(GenerationStaleAfter + time.Second)
	adopted, evicted, err = r.Claim(7, "gen-b")
	if err != nil || !adopted || !evicted {
		t.Fatalf("takeover: adopted=%v evicted=%v err=%v", adopted, evicted, err)
	}
	if r.Current(7, "gen-a") {
		t.Fatal("zombie generation still current (its responds must 404)")
	}
	if !r.Online(7) {
		t.Fatal("new generation not online after takeover")
	}
}

func TestGenerationClaimSameTokenRefreshes(t *testing.T) {
	now := time.Now()
	r := NewGenerationRegistry()
	r.now = func() time.Time { return now }
	if _, _, err := r.Claim(7, "gen-a"); err != nil {
		t.Fatal(err)
	}
	// The same generation re-polling is a lease refresh, not a takeover.
	adopted, evicted, err := r.Claim(7, "gen-a")
	if err != nil || adopted || evicted {
		t.Fatalf("reclaim: adopted=%v evicted=%v err=%v", adopted, evicted, err)
	}
	now = now.Add(GenerationStaleAfter - time.Second)
	if !r.Online(7) {
		t.Fatal("reclaim did not refresh the lease")
	}
	// Unknown keys are offline.
	if r.Online(99) {
		t.Fatal("unknown key reported online")
	}
}

func TestRegisterPollCancelWakes(t *testing.T) {
	r := NewGenerationRegistry()
	if _, _, err := r.Claim(7, "gen-a"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	unregister, ok := r.RegisterPoll(7, "gen-a", cancel)
	if !ok {
		t.Fatal("register failed for the live generation")
	}
	defer unregister()

	// Revocation wakes the poll with the cause (instant 410, design §5.1).
	r.CancelPolls(7, ErrRevoked)
	if !errors.Is(context.Cause(ctx), ErrRevoked) {
		t.Fatalf("poll not woken with the revocation cause: %v", context.Cause(ctx))
	}

	// A dropped (revoked) generation cannot park new polls.
	r.Drop(7)
	if _, ok := r.RegisterPoll(7, "gen-a", cancel); ok {
		t.Fatal("dropped generation registered a poll")
	}
	if r.Online(7) {
		t.Fatal("dropped generation still online")
	}
}
