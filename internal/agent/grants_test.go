package agent

// Session grants (design §3.3): an "allow for session" approval answer keys
// the grant by Bash command prefix, WebFetch domain or WebSearch query prefix,
// and every later gate evaluation consults the session's grants AFTER the
// explicit permission rules (deny and ask always win) and BEFORE the mode
// arms. These tests pin the key shapes, the mutators' copy-on-write contract
// (Task 2's aliasing fix), the gate ordering, and speculativeAllowed's grant
// consultation (§3.1: "grants consulted here AND in speculativeAllowed").

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/permissions"
	"github.com/projanvil/woca/internal/tools"
)

func TestGrantKeyShapes(t *testing.T) {
	cases := []struct {
		name string
		call messages.ToolCall
		want string
	}{
		{
			name: "bash first two segments plus star",
			call: messages.ToolCall{Name: "Bash", Args: map[string]any{"command": "git push --force origin main"}},
			want: "bash:git push*",
		},
		{
			name: "bash single segment still grantable",
			call: messages.ToolCall{Name: "Bash", Args: map[string]any{"command": "make"}},
			want: "bash:make*",
		},
		{
			name: "bash whitespace normalized",
			call: messages.ToolCall{Name: "Bash", Args: map[string]any{"command": "  ls   -la /tmp "}},
			want: "bash:ls -la*",
		},
		{
			name: "bash empty command not grantable",
			call: messages.ToolCall{Name: "Bash", Args: map[string]any{"command": "   "}},
			want: "",
		},
		{
			name: "bash missing command not grantable",
			call: messages.ToolCall{Name: "Bash", Args: map[string]any{}},
			want: "",
		},
		{
			name: "webfetch host",
			call: messages.ToolCall{Name: "WebFetch", Args: map[string]any{"url": "https://example.com/a/b?x=1"}},
			want: "domain:example.com",
		},
		{
			name: "webfetch port not part of the domain key",
			call: messages.ToolCall{Name: "WebFetch", Args: map[string]any{"url": "http://localhost:3000/docs"}},
			want: "domain:localhost",
		},
		{
			// Task 3 review ruling #2: hosts are case-insensitive; the key is
			// lowercased so an approval stored from one casing matches every
			// later casing of the same site.
			name: "webfetch host lowercased into the key",
			call: messages.ToolCall{Name: "WebFetch", Args: map[string]any{"url": "https://EXAMPLE.com/a/b?x=1"}},
			want: "domain:example.com",
		},
		{
			name: "webfetch unparseable url not grantable",
			call: messages.ToolCall{Name: "WebFetch", Args: map[string]any{"url": "not a url"}},
			want: "",
		},
		{
			name: "websearch first two query words",
			call: messages.ToolCall{Name: "WebSearch", Args: map[string]any{"query": "latest AI news today"}},
			want: "websearch:latest AI*",
		},
		{
			name: "websearch single word",
			call: messages.ToolCall{Name: "WebSearch", Args: map[string]any{"query": "gophers"}},
			want: "websearch:gophers*",
		},
		{
			name: "websearch empty query not grantable",
			call: messages.ToolCall{Name: "WebSearch", Args: map[string]any{"query": "  "}},
			want: "",
		},
		{
			name: "other tools not grantable",
			call: messages.ToolCall{Name: "Write", Args: map[string]any{"file_path": "x", "content": "y"}},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GrantKey(tc.call); got != tc.want {
				t.Fatalf("GrantKey(%s) = %q, want %q", tc.call.Name, got, tc.want)
			}
		})
	}
}

// TestGrantMutatorsCopyOnWrite pins the SessionPolicySource grant contract:
// snapshots returned by For never share the grants map with the live entry
// (Task 2's For aliased the map), mutations replace the map (copy-on-write),
// the unnamed ("") session writes the inherited base, and a named session's
// grants are isolated once its entry exists.
func TestGrantMutatorsCopyOnWrite(t *testing.T) {
	src := NewPerSessionPolicySource(config.ModeDefault)

	src.AddGrant("", "bash:make*")
	if got := src.ListGrants(""); !slices.Equal(got, []string{"bash:make*"}) {
		t.Fatalf("ListGrants(\"\") = %v, want [bash:make*]", got)
	}

	// Snapshot isolation: a policy returned by For must not observe (or
	// alias) later mutations — the exact Task 2 aliasing defect.
	p := src.For("")
	src.AddGrant("", "bash:go test*")
	if _, leaked := p.Grants["bash:go test*"]; leaked {
		t.Fatal("For aliased the live grants map: a later AddGrant showed up in the snapshot")
	}
	if got := src.ListGrants(""); !slices.Equal(got, []string{"bash:go test*", "bash:make*"}) {
		t.Fatalf("ListGrants(\"\") after second grant = %v, want sorted [bash:go test* bash:make*]", got)
	}

	// A named session without an entry reads the base (the inheritance rule,
	// same as For)...
	if got := src.ListGrants("s1"); !slices.Equal(got, []string{"bash:go test*", "bash:make*"}) {
		t.Fatalf("ListGrants(s1) = %v, want the base grants while s1 has no entry", got)
	}
	// ...and creating the entry (any named-session write) freezes that
	// inheritance: later base grants must not reach it.
	src.SetMode("s1", config.ModeReadOnly)
	src.AddGrant("", "domain:example.com")
	if got := src.ListGrants("s1"); slices.Contains(got, "domain:example.com") {
		t.Fatalf("base grant leaked into session s1: %v", got)
	}
	src.AddGrant("s1", "websearch:go*")
	if got := src.ListGrants(""); slices.Contains(got, "websearch:go*") {
		t.Fatalf("session grant leaked into the base: %v", got)
	}

	// Revoke removes exactly the named key.
	src.RevokeGrant("s1", "websearch:go*")
	if got := src.ListGrants("s1"); !slices.Equal(got, []string{"bash:go test*", "bash:make*"}) {
		t.Fatalf("ListGrants(s1) after revoke = %v, want the two seeded keys", got)
	}
	// Revoking an absent key is a no-op — and must not create an entry (a
	// created entry would freeze the base and hide later base grants).
	src.RevokeGrant("s1", "domain:nope.example")
	src.RevokeGrant("never-seen", "bash:make*")
	src.AddGrant("", "bash:late*")
	if got := src.ListGrants("never-seen"); !slices.Contains(got, "bash:late*") {
		t.Fatalf("revoke created an entry for never-seen (later base grant invisible): %v", got)
	}

	// The empty key is unmatchable by construction (GrantKey returns "" for
	// non-grantable tools); mutators must not store it.
	src.AddGrant("", "")
	if got := src.ListGrants(""); slices.Contains(got, "") {
		t.Fatalf("empty grant key stored: %v", got)
	}
}

// TestDefaultSourceGrantsAgentGlobal pins the default adapter: without an
// injected store, grants live on the agent's own fields and the gate sees
// them — the TUI/CLI single-session surface keeps working.
func TestDefaultSourceGrantsAgentGlobal(t *testing.T) {
	a := &Agent{mode: config.ModeDefault, perms: nil}
	src := a.PolicySource()
	call := messages.ToolCall{ID: "1", Name: "Bash", Args: map[string]any{"command": "make test"}}
	src.AddGrant("", GrantKey(call))
	if got := src.ListGrants(""); !slices.Equal(got, []string{GrantKey(call)}) {
		t.Fatalf("default source ListGrants = %v", got)
	}
	// Snapshot isolation through the adapter too.
	p := src.For("")
	src.AddGrant("", "bash:other*")
	if _, leaked := p.Grants["bash:other*"]; leaked {
		t.Fatal("default source For aliased the live grants map")
	}
	if ok, err := a.checkPermission(t.Context(), call); err != nil || !ok {
		t.Fatalf("granted call via default source = %v/%v, want allowed", ok, err)
	}
}

// TestGrantSuppressesReAsk pins the once-vs-session behavior end to end: the
// first non-read-only Bash call asks; once the session carries the call's
// grant key (here pre-seeded through the mutator — the approval envelope
// learns to answer Grant=session in Task 5), an identical second call runs
// without asking.
func TestGrantSuppressesReAsk(t *testing.T) {
	workdir := t.TempDir()
	marker := filepath.Join(workdir, "marker.txt")
	bashCall := func(id string) messages.ToolCall {
		return messages.ToolCall{ID: id, Name: "Bash", Args: map[string]any{"command": "touch marker.txt"}}
	}
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{bashCall("1")}},
			messages.AI("did it once"),
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{bashCall("2")}},
			messages.AI("did it twice"),
		),
	)
	src := NewPerSessionPolicySource(config.ModeDefault)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		PolicySource:   src,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Round 1: default mode + a non-read-only command must ask.
	_, err = a.Invoke(t.Context(), []messages.Message{messages.Human("touch the marker")}, "grants-t1")
	ie := interruptFromError(t, err)
	req, ok := ie.Value.(ApprovalRequest)
	if !ok || len(req.Asks) != 1 || req.Asks[0].ToolName != "Bash" {
		t.Fatalf("round 1 interrupt = %#v, want one Bash ask", ie.Value)
	}
	// Approve for the session THROUGH the envelope (Task 5's Grant field).
	if _, err := a.Resume(t.Context(), "grants-t1", ApprovalAnswer{Decisions: map[string]AskDecision{
		req.Asks[0].CallID: {Outcome: OutcomeAllow, Grant: GrantSession},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("approved round-1 command did not run: %v", err)
	}

	// Seed the session grant exactly as the Task 5 "allow for session"
	// answer will (the same mutator write).
	src.AddGrant("", GrantKey(bashCall("2")))

	// Round 2: the identical command must not ask again.
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("touch it again")}, "grants-t2")
	if err != nil {
		t.Fatalf("second identical call interrupted or failed: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("granted round-2 command did not run: %v", err)
	}
	if got := out[len(out)-1].Content; got != "did it twice" {
		t.Fatalf("round 2 final message = %q", got)
	}
}

// TestPlainAllowAnswerIsOncePerCall pins the once-semantics of a plain allow
// (Task 3 review ruling #3): a permission ask answered "y" WITHOUT a grant
// permits only that call — the identical next call asks again. Today's
// behavior, pinned so Task 5's approval envelope cannot regress it: only an
// explicit "allow for session" answer may add a grant.
func TestPlainAllowAnswerIsOncePerCall(t *testing.T) {
	workdir := t.TempDir()
	marker := filepath.Join(workdir, "marker.txt")
	bashCall := func(id string) messages.ToolCall {
		return messages.ToolCall{ID: id, Name: "Bash", Args: map[string]any{"command": "touch marker.txt"}}
	}
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{bashCall("1")}},
			messages.AI("did it once"),
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{bashCall("2")}},
			messages.AI("did it twice"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Round 1: default mode + a non-read-only command asks; answer a plain
	// allow (Grant stays none).
	_, err = a.Invoke(t.Context(), []messages.Message{messages.Human("touch the marker")}, "once-t1")
	ie := interruptFromError(t, err)
	req, ok := ie.Value.(ApprovalRequest)
	if !ok || len(req.Asks) != 1 || req.Asks[0].ToolName != "Bash" {
		t.Fatalf("round 1 interrupt = %#v, want one Bash ask", ie.Value)
	}
	if _, err := a.Resume(t.Context(), "once-t1", ApprovalAnswer{Decisions: map[string]AskDecision{
		req.Asks[0].CallID: {Outcome: OutcomeAllow},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("approved round-1 command did not run: %v", err)
	}

	// Round 2: the identical command must ask again — the plain allow created
	// no session grant.
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	_, err = a.Invoke(t.Context(), []messages.Message{messages.Human("touch it again")}, "once-t2")
	ie = interruptFromError(t, err)
	if req, ok := ie.Value.(ApprovalRequest); !ok || len(req.Asks) != 1 || req.Asks[0].ToolName != "Bash" {
		t.Fatalf("round 2 interrupt = %#v, want a second Bash ask (plain allow is once)", ie.Value)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("SESSION GRANT LEAK: the second identical call ran without asking after a plain once-allow")
	}
}

// TestDenyRuleBeatsSessionGrant pins review-focus #2: a session grant must
// never override an explicit deny rule — the gate consults grants only after
// the explicit rules.
func TestDenyRuleBeatsSessionGrant(t *testing.T) {
	src := NewPerSessionPolicySource(config.ModeDefault)
	a := &Agent{mode: config.ModeDefault, perms: &permissions.Rules{Deny: []string{"Bash"}}}
	a.policy = src
	call := messages.ToolCall{ID: "d1", Name: "Bash", Args: map[string]any{"command": "touch x.txt"}}
	src.AddGrant("", GrantKey(call))
	if ok, err := a.checkPermission(t.Context(), call); err != nil || ok {
		t.Fatalf("deny rule vs session grant = %v/%v, want denied without asking", ok, err)
	}
	// Same ordering in speculativeAllowed: a denied call never pre-starts,
	// grant or not.
	if a.speculativeAllowed(t.Context(), call) {
		t.Fatal("denied call speculatively dispatched despite the deny rule")
	}
}

// TestAskRuleStillAsksWithGrant: an explicit ask rule wins over a session
// grant — the grant may only lift the mode-arm ask, never an explicit ask.
func TestAskRuleStillAsksWithGrant(t *testing.T) {
	src := NewPerSessionPolicySource(config.ModeDefault)
	a := &Agent{mode: config.ModeDefault, perms: &permissions.Rules{Ask: []string{"Bash"}}}
	a.policy = src
	call := messages.ToolCall{ID: "a1", Name: "Bash", Args: map[string]any{"command": "make test"}}
	src.AddGrant("", GrantKey(call))
	askOutsideRun(t, func() {
		_, _ = a.checkPermission(t.Context(), call)
	})
	// And speculativeAllowed keeps refusing to pre-run an ask-rule call.
	if a.speculativeAllowed(t.Context(), call) {
		t.Fatal("ask-rule call speculatively dispatched despite the ask rule")
	}
}

// TestSpeculativeAllowedHonorsGrant pins §3.1's "grants consulted here AND in
// speculativeAllowed": grants occupy the same slot as in the gate — after the
// explicit rules, before the read-only classification — and never lift the
// read-only speculation gate.
func TestSpeculativeAllowedHonorsGrant(t *testing.T) {
	src := NewPerSessionPolicySource(config.ModeDefault)
	a := &Agent{mode: config.ModeDefault, perms: nil}
	a.policy = src
	ctx := WithSessionPolicy(t.Context(), "spec", src)

	// A classifier-read-only Bash with a grant still speculates (it did
	// before grants; the consultation must not break it).
	roBash := messages.ToolCall{ID: "1", Name: "Bash", Args: map[string]any{"command": "git status --short"}}
	src.AddGrant("spec", GrantKey(roBash))
	if !a.speculativeAllowed(ctx, roBash) {
		t.Fatal("granted classifier-read-only Bash must still speculate")
	}
	if ok, err := a.checkPermission(ctx, roBash); err != nil || !ok {
		t.Fatalf("granted read-only bash at the gate = %v/%v, want allowed", ok, err)
	}

	// A granted WebFetch speculates (CatWeb is classifier-read-only).
	fetch := messages.ToolCall{ID: "2", Name: "WebFetch", Args: map[string]any{"url": "https://example.com/x"}}
	src.AddGrant("spec", GrantKey(fetch))
	if !a.speculativeAllowed(ctx, fetch) {
		t.Fatal("granted WebFetch must speculate")
	}

	// A granted NON-read-only call never speculates: speculation stays
	// read-only-gated whatever permitted it (a restarted stream would have
	// executed the call twice) — the grant only removes the gate's ask.
	write := messages.ToolCall{ID: "3", Name: "Bash", Args: map[string]any{"command": "touch w.txt"}}
	src.AddGrant("spec", GrantKey(write))
	if a.speculativeAllowed(ctx, write) {
		t.Fatal("granted non-read-only call must not speculate")
	}
	if ok, err := a.checkPermission(ctx, write); err != nil || !ok {
		t.Fatalf("granted write at the gate = %v/%v, want allowed (the grant replaces the ask)", ok, err)
	}

	// Grants stay session-scoped: another session's grant is invisible to
	// this run's policy.
	other := messages.ToolCall{ID: "4", Name: "Bash", Args: map[string]any{"command": "rm -rf /tmp/nope"}}
	src.AddGrant("other", GrantKey(other))
	if _, leaked := src.For("spec").Grants[GrantKey(other)]; leaked {
		t.Fatal("session 'other' grant leaked into session 'spec'")
	}
	if a.speculativeAllowed(ctx, other) {
		t.Fatal("cross-session grant enabled speculation")
	}
}

// TestPlanBeatsBypassDirect (parked from Task 4): enforced plan mode
// overrides even bypass — the pure gate evaluator must route a non-read-only
// call to ask (not allow) while the plan flag is on, whatever the session's
// base mode. The hooks node turns that ask into an ApprovalRequest ask; it
// must never silently allow under bypass.
func TestPlanBeatsBypassDirect(t *testing.T) {
	src := NewPerSessionPolicySource(config.ModeBypass)
	a := &Agent{mode: config.ModeBypass}
	a.policy = src
	ctx := WithSessionPolicy(t.Context(), "plan-bypass", src)
	call := messages.ToolCall{ID: "pb1", Name: "Bash", Args: map[string]any{"command": "touch x.txt"}}
	src.SetPlan("plan-bypass", true)

	allow, _ := a.evalPermission(ctx, call)
	if allow {
		t.Fatal("PLAN LEAK: plan=true over bypass allowed a non-read-only call (plan outranks the mode)")
	}

	// Once the plan flag flips off (ExitPlanMode approved), bypass allows
	// the same call outright.
	src.SetPlan("plan-bypass", false)
	allow, ask := a.evalPermission(ctx, call)
	if !allow || ask {
		t.Fatalf("bypass after plan approval = allow=%v ask=%v, want allowed", allow, ask)
	}
}

// TestSpeculativeGrantPlanClampDirect (parked from Task 4): a session grant
// never lifts the plan-mode restriction in the speculative path either —
// speculativeAllowed keeps its read-only clamp while the plan flag is on,
// and grants do not punch through.
func TestSpeculativeGrantPlanClampDirect(t *testing.T) {
	src := NewPerSessionPolicySource(config.ModeDefault)
	a := &Agent{mode: config.ModeDefault}
	a.policy = src
	ctx := WithSessionPolicy(t.Context(), "plan-clamp", src)
	write := messages.ToolCall{ID: "pc1", Name: "Bash", Args: map[string]any{"command": "touch y.txt"}}
	read := messages.ToolCall{ID: "pc2", Name: "Bash", Args: map[string]any{"command": "git status --short"}}
	src.AddGrant("plan-clamp", GrantKey(write))
	src.AddGrant("plan-clamp", GrantKey(read))
	src.SetPlan("plan-clamp", true)

	if a.speculativeAllowed(ctx, write) {
		t.Fatal("PLAN LEAK: granted non-read-only call speculated under plan=true")
	}
	if allow, ask := a.evalPermission(ctx, write); allow {
		t.Fatalf("PLAN LEAK: granted non-read-only call allowed (ask=%v) under plan=true — plan must deny before grants lift", ask)
	}
	// Read-only stays fine under plan, grant or no grant.
	if !a.speculativeAllowed(ctx, read) {
		t.Fatal("granted classifier-read-only call must still speculate under plan=true")
	}
}
