# Personal Remote Relay (woca-relay) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the personal `woca` binary remote reachability: a new `woca-relay` binary on a public VPS relays authenticated browser/native-client traffic to a NAT-hidden `woca` over an outbound HTTP long-poll tunnel, keyed by symmetric `wcr_` rendezvous keys with instant two-sided revocation.

**Architecture:** Two new components plus wiring. (1) `woca-relay` (`cmd/woca-relay` + `internal/relay`): sqlite key/session store (SHA-256 hashes only), rate-limited login (cookie + body token), SPA serving, per-key parking queue (cap 64) with generation leases (45s staleness, 409 on conflict), `GET /tunnel/poll` + `POST /tunnel/respond/{id}` tunnel endpoints, per-chunk flush streaming with hop-by-hop header filtering, revocation cascade with append-only audit log. (2) `internal/relayclient` compiled into `woca`: 4-worker poll loop (backoff 1s→60s ±20% jitter, new generation per reconnect, terminal on 401/410), in-process execution against `server.Handler()` via a `streamRecorder` rewrite (headers freeze at first Write, Flush no-op), chunked respond upload, mandatory SSE heartbeat (`: ping\n\n` after 20s idle). (3) Wiring: `woca relay connect|status|disconnect` CLI persisting via `internal/settings` secrets, `/api/relay/status` local API, serve lifecycle through the existing `servefront.Edition` seams; native-client static-Bearer `CredentialProvider` + relay profile token storage; Makefile/CI gates and a loopback e2e script.

**Tech Stack:** Go 1.26 stdlib (`net/http`, `io.Pipe`, `crypto/sha256`, `crypto/rand`, `encoding/base64/json`, `database/sql`), `modernc.org/sqlite` (direct import, already in `ci/deps-personal.txt`), `github.com/rs/zerolog` via `internal/logging`, `github.com/spf13/cobra`. Zero new third-party dependencies.

## Global Constraints

- No `internal/ee` imports anywhere in the new code (`make check-editions` enforced; enterprise tunnel code is a semantics reference only — rewrite, never import).
- Zero new third-party dependencies: stdlib + `modernc.org/sqlite` + `zerolog` only; `golang.org/x/time/rate` is FORBIDDEN (rate limiter is hand-rolled stdlib).
- Key prefix `wcr_` + 32-byte CSPRNG base64url (256-bit entropy); full key printed exactly once at `key create`.
- Revocation error code is exactly `"revoked"` (never `key_revoked`) — the SPA/native frontend contract (WocaApp.vue:137, transport.ts:48-50, streaming.ts:151-157, `authn.CodeRevoked`).
- Client request body cap 4 MB (`MaxClientBody = 4 << 20`) → 413.
- Parking queue cap 64 per key → 503 when full.
- Generation lease: 45 s staleness (`GenerationStaleAfter = 45 * time.Second`); a competing live generation gets 409; zombie generation responds get 404.
- Sessions: 7-day sliding (idle) + 30-day absolute cap; session tokens stored as SHA-256 hashes only.
- Session cookie name `woca_relay_session`; `HttpOnly`; `SameSite=Lax`; `Secure` from direct TLS or (only with `--behind-proxy`) `X-Forwarded-Proto`.
- SHA-256 hashing for keys and session tokens (high-entropy tokens; no bcrypt/argon2).
- `modernc.org/sqlite` imported directly by `internal/relay`; NEVER `internal/wocadb` (it blank-imports pgx — personal-edition dependency hygiene).
- Rate limiter stdlib-only: 10 login attempts/minute/IP, bounded per-IP table with lazy eviction.
- Poll wait default 25 s; respond upload chunked; relay flushes every chunk; SSE heartbeat `: ping\n\n` injected after 20 s idle (bridge side, mandatory).
- relayclient backoff 1 s doubling to 60 s with ±20% jitter; 4 poll workers; new 128-bit generation per reconnect; 401/410 are terminal (no reconnect).
- TLS fail-closed: public bind without TLS refuses to start; `--insecure` is explicit dev-only; `X-Forwarded-*` trusted only with `--behind-proxy`.
- Anonymous route whitelist (relay): SPA static assets, `/api/health`, `POST /api/auth/login` only.
- Hop-by-hop headers stripped on respond: `Connection` (plus its named tokens), `Keep-Alive`, `Transfer-Encoding`, `TE`, `Trailer`, `Upgrade`; `X-Accel-Buffering: no` set on event-stream responses.
- `Cookie`/`Authorization` stripped before parking; all `X-Woca-*` headers stripped before bridge execution; `X-Woca-Relay-Remote: 1` injected by the bridge process only.

## Assumptions / Deviations

Places where this plan resolves a spec ambiguity or deliberately deviates from a literal spec reading. Each is also marked at the point of implementation.

1. **CLI revoke is cross-process; "instant" means ≤ 1 s.** `woca-relay key revoke` is a separate OS process from `woca-relay serve`, so it cannot cancel in-memory polls directly. The CLI writes `revoked_at` in sqlite; the serve process runs `Revoker.Watch` on a 1 s interval, observes the row, and runs the live cascade (Task 6). In-process (API-driven, future) revokes cascade in the same call. "立即" in spec §5 therefore reads as: immediate within the relay process, ≤ 1 s for CLI-issued revokes.
2. **Post-revoke session handling: tombstone, not delete (deviation from spec §5.2's "删除").** The cascade deliberately does NOT call `DeleteSessionsForKey`. Deleting the rows would make post-revoke requests resolve to `ErrSessionUnknown` → 401 `{"code":"unauthorized"}`, indistinguishable from logged-out — and the SPA's revoked notice (WocaApp.vue:137) only fires on code `"revoked"`. Tombstoned rows can never authenticate: `ResolveSession`'s keys join returns `ErrKeyRevoked` before any expiry check, so every post-revoke request (cookie or Bearer) determinably gets 401 `{"code":"revoked"}`. Expired tombstones are lazily purged by `ResolveSession`'s existing delete-on-expiry path; `DeleteSessionsForKey` remains as store API for maintenance (see Task 2 comment and Task 6 `Revoker.Revoke`).
3. **`woca-relay key list` "bridge online" is derived cross-process.** The generation registry is in-memory in the serve process, so the CLI cannot read it. Tunnel poll/respond stamp `keys.last_used_at` (`TouchKey`); the CLI renders "online" when `last_used_at` is within the 45 s lease window (`GenerationStaleAfter`). Tunnel auth is the only `TouchKey` caller, so the semantics stay "bridge activity", not client activity.
4. **`user_id` = first 8 hex chars of the stored SHA-256 key hash** (spec §3.3's "哈希前 8 位"), computed as `Key.UserID()` / in `ResolveSession` — stable, and derived from the hash rather than the raw key.
5. **e2e uses `GET /api/events` instead of launching a real run.** A run needs a configured model provider, which loopback CI does not have; the e2e script still exercises create key → connect → login → tunneled API → SSE headers (`Content-Type`, `X-Accel-Buffering`, hop-by-hop filtering) → revoke → both-side errors. Frame-level SSE fidelity through the tunnel (chunk immediacy, heartbeat, content-type survival) is covered by `TestTunnelSSE` (Task 5) and `TestFullChain` (Task 7).
6. **`expires_at` is Unix milliseconds** in the login body (edgegw precedent, edgegw/auth.go's `time.UnixMilli(out.ExpiresAt)`; `web/src/api/bridge.ts:9` types it as a plain number).
7. **Task 3's skeleton test assertion changes in Task 5 (404 → 401).** In the skeleton, `GET /api/models` 404s through `webfx.SPA`; once Task 5 attaches the tunnel plane, every `/api/` path is session-gated first, so the same request returns 401. Task 5 carries this as its own discrete edit block (Step 4, edit (b)).
8. **`internal/relayclient/integration_test.go` imports `internal/relay` — test-only.** `go list -deps ./cmd/woca` excludes other packages' test imports, so the personal dependency baseline is unaffected; Task 9 verifies this with `make check-deps` and the `comm -13` zero-new-third-party check.
9. **Spec §6.5/§9 "停放中 relay 重启 → client 请求 504" is reinterpreted.** A dead relay process cannot answer 504 to anyone — when the relay restarts, parked client requests see a connection reset/close, not an HTTP status. The SPA's existing reconnect/resync path (SSE Last-Event-ID, sse.go:45-59) and request retry behavior cover the client side; the bridge side behaves exactly as specified (respond POST failure → `CloseWithError` unsticks the handler goroutine; poll reconnects with a new generation). No relay-side compensation is implemented for the 504 row, and none is possible.

## File Structure

### New files

| File | Responsibility |
|---|---|
| `internal/relay/keystore.go` | sqlite open + `schema_migrations` migration; `keys` table CRUD; `wcr_` key minting; SHA-256 `HashToken`; `LookupKey`/`RevokeKey`/`ListKeys`/`TouchKey` (Tasks 1; extended in 5/6) |
| `internal/relay/session.go` | `sessions` table lifecycle: `CreateSession`/`ResolveSession` (7d sliding + 30d absolute)/`DeleteSession`/`DeleteSessionsForKey` (Task 2) |
| `internal/relay/ratelimit.go` | Bounded stdlib per-IP token bucket (10/min, capped table + lazy eviction) (Task 2) |
| `internal/relay/errors.go` | JSON error envelope `WriteError`/`writeJSON` + wire codes (`unauthorized`, `revoked`, `bridge_offline`) (Task 2) |
| `internal/relay/authhttp.go` | `AuthService`: `POST /api/auth/login`, `/logout`, `GET /api/auth/me`, `SessionFromRequest`, proxy-aware `ClientIP`/`SecureCookie` (Task 2; audit field added in 6) |
| `internal/relay/serve.go` | `Relay` assembly + `Handler()` routing, `CheckBind` fail-closed + `isPublicBind`, revocation attach point (Tasks 3, modified 5/6) |
| `internal/relay/park.go` | Per-key parking queue: `Park`/`Take`/`Complete`/`FailAll`, cap 64, void-on-disconnect (Task 4) |
| `internal/relay/generation.go` | Generation registry: `Claim` (409/45s eviction)/`Current`/`Online`/`RegisterPoll`/`CancelPolls`/`Drop` (Task 4) |
| `internal/relay/tunnel.go` | `TunnelService`: `GET /tunnel/poll`, `POST /tunnel/respond/{id}`, header base64-JSON decode + hop-by-hop strip (Task 5) |
| `internal/relay/forward.go` | Client request forwarding: offline 503, 4MB cap, credential strip, park + per-chunk flush streaming, SSE revocation injection (Task 5) |
| `internal/relay/audit.go` | Append-only JSON-lines audit log (login failures, revocations, bridge up/down) (Task 6) |
| `internal/relay/revoke.go` | `Revoker`: cascade (wake polls, drop generation, delete sessions, drain queue, audit) + `Sweep`/`Watch` for CLI-issued revokes (Task 6) |
| `internal/relay/*_test.go` | Per-task tests mirroring `internal/ee/edgegw/edgegw_test.go` in-process assembly |
| `cmd/woca-relay/main.go` | cobra root, persistent `--db` flag (env `WOCA_RELAY_DB`), `openKeys` helper (Task 1; serve registration in 3) |
| `cmd/woca-relay/key.go` | `woca-relay key create|list|revoke` (Task 1) |
| `cmd/woca-relay/serve.go` | `woca-relay serve`: TLS/`--insecure`/`--behind-proxy` bind policy + webfx router (Task 3; audit+revoker wiring in 6) |
| `internal/relayclient/wire.go` | Tunnel wire vocabulary (duplicated constants + `pollPayload`, no import of `internal/relay`) (Task 7) |
| `internal/relayclient/recorder.go` | `streamRecorder` same-semantics rewrite (freeze at first Write/WriteHeader, Flush no-op, drop Content-Length) (Task 7) |
| `internal/relayclient/heartbeat.go` | SSE heartbeat writer (20 s idle → `: ping\n\n`, pipe-safe, ctx-cancel exit) (Task 7) |
| `internal/relayclient/client.go` | `Client`: worker pool, poll loop with backoff/jitter/generation rotation, `execute` against in-process handler, chunked `respond` (Task 7) |
| `internal/relayclient/persist.go` | Relay config persistence via `internal/settings` (URL plain row, key encrypted secret, env overrides) (Task 8) |
| `internal/relayclient/service.go` | `Service` lifecycle: `SetHandler`/`SetSettings`/`Run`/`Disconnect`/`Status` + local HTTP status/disconnect handlers (Task 8) |
| `internal/relayclient/*_test.go` | recorder/heartbeat/client tests + full-chain integration against real `internal/relay` (Tasks 7-8) |
| `native-client/internal/transport/relaycred.go` | `StaticBearer` `CredentialProvider` for relay profiles (Task 9) |
| `native-client/internal/transport/relaycred_test.go` | StaticBearer contract test (Task 9) |
| `native-client/internal/configstore/relaytoken_test.go` | Relay token storage round trip (Task 9) |
| `ci/deps-relay.txt` | Dependency baseline for `./cmd/woca-relay` (generated; Task 9) |
| `ci/e2e-relay.sh` | Loopback end-to-end: create key → connect → login → tunneled API + SSE → revoke (Task 9) |

### Modified files

| File | Change |
|---|---|
| `internal/settings/settings.go:68-79` | Add `SecretRelayKey` secret name (Task 8) |
| `internal/settings/settings.go:84-120` | Add `RelayURL` field to `Settings` (Task 8) |
| `internal/settings/settings.go:367-504` | `applyPairs`: read `relay_url` row (Task 8) |
| `internal/settings/settings.go:542-555` | `Save`: persist `relay_url` (Task 8) |
| `internal/settings/keys.go:11-62` | Add `KeyRelayURL` constant (Task 8) |
| `internal/settings/keys.go:231-422` | `Apply`: `relay-url` case (Task 8) |
| `internal/settings/keys.go:484-568` | `KeyValue`: `relay-url` case (Task 8) |
| `cmd/woca/main.go:16-59` | Imports: `authn`, `relayclient` (Task 8) |
| `cmd/woca/main.go:121-128` | Serve `Edition`: `Handler`/`OnSettings`/`Start`/`Routes` wiring + `newRelayCmd` registration (Task 8) |
| `cmd/woca/main.go` (append after `newConfigCmd`, :326) | `newRelayCmd` + status helpers (Task 8) |
| `native-client/internal/configstore/store.go:176-214` | `SetProfileRelayToken`/`ProfileRelayToken` preference rows (Task 9) |
| `native-client/internal/configstore/store.go:298-324` | `DeleteProfile`: also delete the relay token row (Task 9) |
| `native-client/main.go:156-170` | Credentials chain: relay profile → `StaticBearer` (Task 9) |
| `Makefile:1` | `.PHONY`: add `build-relay e2e-relay` (Task 9) |
| `Makefile:31-33` | Add `build-relay` target after `build` (Task 9) |
| `Makefile:39` | `build-personal: web build build-relay` (Task 9) |
| `Makefile:105-112` | Extend `deps-baseline` + `check-deps` for `./cmd/woca-relay` (Task 9) |
| `Makefile:372` | `check-editions` scan: add `./cmd/woca-relay` (Task 9) |
| `Makefile` (end, after :378) | `e2e-relay` target (Task 9) |
| `internal/relay/keystore.go` | Task 6 adds `ListRevokedKeys` + `KeyByID` |
| `internal/relay/serve.go` | Task 5 attaches tunnel plane + forwarding route; Task 6 adds `AttachRevocation` |
| `internal/relay/authhttp.go` | Task 6 adds `Audit` field + login audit events |
| `cmd/woca-relay/main.go` | Task 3 registers `serve` subcommand |
| `cmd/woca-relay/serve.go` | Task 6 wires audit log + revocation watcher |

---

### Task 1: Relay KeyStore + sqlite schema + `woca-relay key` CLI

**Files:**
- Create: internal/relay/keystore.go
- Create: internal/relay/keystore_test.go
- Create: cmd/woca-relay/main.go
- Create: cmd/woca-relay/key.go
- Test: internal/relay/keystore_test.go

**Interfaces:**
- Consumes: nothing from other tasks (foundation task). Uses `modernc.org/sqlite` driver name `"sqlite"` (same as `native-client/internal/configstore/store.go:95`), `github.com/spf13/cobra` (root-command conventions from `cmd/woca-edge/main.go:35-164`: `SilenceUsage`/`SilenceErrors`, env-to-flag backfill loop).
- Produces (relied on by every later task):
  - `const KeyPrefix = "wcr_"`, `const GenerationStaleAfter = 45 * time.Second`
  - `var ErrKeyUnknown, ErrKeyRevoked error`
  - `func GenerateKey() (string, error)`, `func HashToken(token string) []byte`
  - `type Key struct { ID int64; Name string; KeyHash []byte; Prefix string; CreatedAt time.Time; RevokedAt *time.Time; LastUsedAt *time.Time }`, `func (k Key) UserID() string`
  - `func OpenKeyStore(path string) (*KeyStore, error)`, `func (s *KeyStore) Close() error`
  - `func (s *KeyStore) CreateKey(ctx, name) (Key, raw string, err error)`
  - `func (s *KeyStore) LookupKey(ctx, raw) (Key, error)`
  - `func (s *KeyStore) ListKeys(ctx) ([]Key, error)`
  - `func (s *KeyStore) RevokeKey(ctx, name) (Key, bool, error)`
  - `func (s *KeyStore) TouchKey(ctx, id int64)`
  - Test helpers (same package): `openTestKeyStore(t) *KeyStore`
  - CLI: `woca-relay --db <path> key create|list|revoke`

- [ ] **Step 1: Write the failing test**

Write `internal/relay/keystore_test.go`:

```go
package relay

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func openTestKeyStore(t *testing.T) *KeyStore {
	t.Helper()
	ks, err := OpenKeyStore(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ks.Close() })
	return ks
}

func TestGenerateKey(t *testing.T) {
	a, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a, KeyPrefix) {
		t.Fatalf("key %q lacks the %s prefix", a, KeyPrefix)
	}
	// wcr_ + 43 base64url chars (32 bytes, raw — no padding).
	if len(a) != len(KeyPrefix)+43 {
		t.Fatalf("key length %d, want %d", len(a), len(KeyPrefix)+43)
	}
	b, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two mints collided")
	}
}

func TestKeyStoreCreateLookupRevoke(t *testing.T) {
	ks := openTestKeyStore(t)
	ctx := context.Background()

	k, raw, err := ks.CreateKey(ctx, "home-mac")
	if err != nil {
		t.Fatal(err)
	}
	if k.Prefix != raw[:8] {
		t.Fatalf("prefix %q, want the key's first 8 chars", k.Prefix)
	}
	got, err := ks.LookupKey(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != k.ID || got.Name != "home-mac" {
		t.Fatalf("lookup returned %+v", got)
	}
	// user_id derives from the stored hash, never from the raw key.
	if got.UserID() == "" || strings.HasPrefix(raw, got.UserID()) {
		t.Fatalf("user id %q must be hash-derived", got.UserID())
	}
	if _, err := ks.LookupKey(ctx, "wcr_wrong"); !errors.Is(err, ErrKeyUnknown) {
		t.Fatalf("wrong key: %v, want ErrKeyUnknown", err)
	}
	if _, err := ks.LookupKey(ctx, "not-a-key"); !errors.Is(err, ErrKeyUnknown) {
		t.Fatalf("non-wcr token: %v, want ErrKeyUnknown", err)
	}
	if _, _, err := ks.CreateKey(ctx, "home-mac"); err == nil {
		t.Fatal("duplicate name accepted (name is UNIQUE)")
	}

	revoked, ok, err := ks.RevokeKey(ctx, "home-mac")
	if err != nil || !ok {
		t.Fatalf("revoke: err=%v ok=%v", err, ok)
	}
	if revoked.RevokedAt == nil {
		t.Fatal("revoked row lacks revoked_at")
	}
	if _, err := ks.LookupKey(ctx, raw); !errors.Is(err, ErrKeyRevoked) {
		t.Fatalf("revoked lookup: %v, want ErrKeyRevoked", err)
	}
	// Idempotent: unknown or already-revoked returns ok=false, no error.
	if _, ok, err := ks.RevokeKey(ctx, "home-mac"); err != nil || ok {
		t.Fatalf("second revoke: err=%v ok=%v, want ok=false", err, ok)
	}
	if _, ok, err := ks.RevokeKey(ctx, "nope"); err != nil || ok {
		t.Fatalf("unknown revoke: err=%v ok=%v, want ok=false", err, ok)
	}

	if _, _, err := ks.CreateKey(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	list, err := ks.ListKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list len %d, want 2", len(list))
	}
}

func TestKeyStoreReopenPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	ks, err := OpenKeyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := ks.CreateKey(context.Background(), "persist")
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Close(); err != nil {
		t.Fatal(err)
	}
	ks2, err := OpenKeyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ks2.Close() }()
	if _, err := ks2.LookupKey(context.Background(), raw); err != nil {
		t.Fatalf("key lost across reopen: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
go test ./internal/relay/ -count=1
```

Expected failure: `internal/relay/keystore_test.go:8:14: undefined: OpenKeyStore` (package does not exist yet — `no required module provides package` / build constraints exclude all Go files, or `undefined: OpenKeyStore`, `undefined: GenerateKey`, `undefined: KeyPrefix`, `undefined: ErrKeyUnknown`, `undefined: ErrKeyRevoked`).

- [ ] **Step 3: Write minimal implementation**

Write `internal/relay/keystore.go`:

```go
// Package relay implements the personal-edition remote relay (design
// 2026-09-17-personal-relay-design): the key authority (sqlite), client
// sessions, the parking/generation tunnel plane, and the public HTTP
// surface of the woca-relay binary.
//
// Edition boundary: this package never imports internal/ee; its only
// third-party dependencies are modernc.org/sqlite (the storage driver —
// imported directly, never through internal/wocadb, which blank-imports
// pgx) and zerolog (logging). Both are already in the personal dependency
// baseline (ci/deps-personal.txt).
package relay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// KeyPrefix marks personal-relay keys (design §2: woca_ is enterprise
// vocabulary; wcr_ keeps support and log forensics unambiguous).
const KeyPrefix = "wcr_"

// GenerationStaleAfter is the generation lease window (design §6.2): a
// generation with no poll for this long is dead and a new generation
// takes over; bridge-online is the same window (25s poll wait + network
// slack). Declared here because the key CLI's online check shares it.
const GenerationStaleAfter = 45 * time.Second

// Sentinels for key lookups.
var (
	ErrKeyUnknown = errors.New("relay: unknown key")
	ErrKeyRevoked = errors.New("relay: key revoked")
)

// Key is one rendezvous-key row. Raw key material is never stored — only
// its SHA-256 (design §4.1).
type Key struct {
	ID         int64
	Name       string
	KeyHash    []byte
	Prefix     string // first 8 chars of the raw key, display only
	CreatedAt  time.Time
	RevokedAt  *time.Time // nil = live
	LastUsedAt *time.Time
}

// UserID is the key's stable public identifier: the first 8 hex chars of
// its stored hash (design §3.3 — the /api/auth/me shape).
func (k Key) UserID() string {
	if len(k.KeyHash) == 0 {
		return ""
	}
	return hex.EncodeToString(k.KeyHash)[:8]
}

// GenerateKey mints a new rendezvous key: 32 bytes of CSPRNG entropy
// base64url-encoded under the wcr_ prefix (256 bits, design §4.1).
func GenerateKey() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("relay: generate key: %w", err)
	}
	return KeyPrefix + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// HashToken is the lookup digest for keys and session tokens: high-entropy
// random tokens hash with plain SHA-256 (design §4.1 — bcrypt/argon2 are
// password constructions and would only add a DoS surface here). Lookup by
// hash also sidesteps timing comparison concerns.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// KeyStore is the relay's only persistent state: rendezvous keys and
// client sessions. The relay holds zero business data (design §8).
type KeyStore struct {
	db *sql.DB
}

const currentSchemaVersion = 1

// Versioned schema_migrations pattern (template: native-client/
// configstore/store.go:36-41 — copied as a pattern only; that package is
// a separate Go module and must not be imported).
const schemaMigrationsDDL = `CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY,
  applied_at INTEGER NOT NULL
)`

// Schema (design §8).
const keysDDL = `CREATE TABLE keys (
  id           INTEGER PRIMARY KEY,
  name         TEXT NOT NULL UNIQUE,
  key_hash     BLOB NOT NULL UNIQUE,
  prefix       TEXT NOT NULL,
  created_at   TIMESTAMP NOT NULL,
  revoked_at   TIMESTAMP,
  last_used_at TIMESTAMP
)`

const sessionsDDL = `CREATE TABLE sessions (
  token_hash BLOB PRIMARY KEY,
  key_id     INTEGER NOT NULL REFERENCES keys(id),
  created_at TIMESTAMP NOT NULL,
  touched_at TIMESTAMP NOT NULL
)`

// OpenKeyStore opens (creating and migrating as needed) the relay
// database at path.
func OpenKeyStore(path string) (*KeyStore, error) {
	if path == "" {
		return nil, fmt.Errorf("relay: database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("relay: create database directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("relay: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("relay: ping sqlite: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("relay: configure sqlite: %w", err)
	}
	s := &KeyStore{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *KeyStore) Close() error { return s.db.Close() }

func (s *KeyStore) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var version sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version)
	if err != nil {
		// schema_migrations itself is absent: fresh database.
		for _, stmt := range []string{schemaMigrationsDDL, keysDDL, sessionsDDL} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("relay: apply schema: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			currentSchemaVersion, time.Now().Unix()); err != nil {
			return fmt.Errorf("relay: record schema version: %w", err)
		}
		return tx.Commit()
	}
	if int(version.Int64) != currentSchemaVersion {
		return fmt.Errorf("relay: unsupported schema version %d (binary supports %d)",
			version.Int64, currentSchemaVersion)
	}
	return tx.Commit()
}

// CreateKey mints and stores a new key, returning the row and the full
// raw key — the ONLY time the raw key is available (design §4.1).
func (s *KeyStore) CreateKey(ctx context.Context, name string) (Key, string, error) {
	if name == "" || len(name) > 128 {
		return Key{}, "", fmt.Errorf("relay: key name must be 1-128 characters, got %d", len(name))
	}
	raw, err := GenerateKey()
	if err != nil {
		return Key{}, "", err
	}
	prefix := raw
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO keys (name, key_hash, prefix, created_at) VALUES (?, ?, ?, ?)`,
		name, HashToken(raw), prefix, now)
	if err != nil {
		return Key{}, "", fmt.Errorf("relay: create key %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Key{}, "", err
	}
	return Key{ID: id, Name: name, KeyHash: HashToken(raw), Prefix: prefix, CreatedAt: now}, raw, nil
}

const keySelect = `SELECT id, name, key_hash, prefix, created_at, revoked_at, last_used_at FROM keys`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanKey(row rowScanner) (Key, error) {
	var k Key
	var revokedAt, lastUsedAt sql.NullTime
	if err := row.Scan(&k.ID, &k.Name, &k.KeyHash, &k.Prefix, &k.CreatedAt, &revokedAt, &lastUsedAt); err != nil {
		return Key{}, err
	}
	if revokedAt.Valid {
		k.RevokedAt = &revokedAt.Time
	}
	if lastUsedAt.Valid {
		k.LastUsedAt = &lastUsedAt.Time
	}
	return k, nil
}

// LookupKey resolves a raw key by its SHA-256 (design §4.1). Errors:
// ErrKeyUnknown (absent or malformed), ErrKeyRevoked.
func (s *KeyStore) LookupKey(ctx context.Context, raw string) (Key, error) {
	if len(raw) != len(KeyPrefix)+43 || raw[:len(KeyPrefix)] != KeyPrefix {
		return Key{}, ErrKeyUnknown
	}
	k, err := scanKey(s.db.QueryRowContext(ctx, keySelect+` WHERE key_hash = ?`, HashToken(raw)))
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, ErrKeyUnknown
	}
	if err != nil {
		return Key{}, fmt.Errorf("relay: lookup key: %w", err)
	}
	if k.RevokedAt != nil {
		return k, ErrKeyRevoked
	}
	return k, nil
}

// ListKeys returns every key, oldest first (the `key list` CLI).
func (s *KeyStore) ListKeys(ctx context.Context) ([]Key, error) {
	rows, err := s.db.QueryContext(ctx, keySelect+` ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("relay: list keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeKey marks name revoked (design §5). ok is false when the key is
// unknown or already revoked — revoking is idempotent at the CLI level.
func (s *KeyStore) RevokeKey(ctx context.Context, name string) (Key, bool, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE keys SET revoked_at = ? WHERE name = ? AND revoked_at IS NULL`, now, name)
	if err != nil {
		return Key{}, false, fmt.Errorf("relay: revoke key %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Key{}, false, err
	}
	if n == 0 {
		return Key{}, false, nil
	}
	k, err := scanKey(s.db.QueryRowContext(ctx, keySelect+` WHERE name = ?`, name))
	if err != nil {
		return Key{}, false, fmt.Errorf("relay: reload revoked key: %w", err)
	}
	return k, true, nil
}

// TouchKey stamps last_used_at (bridge tunnel activity — the `key list`
// online check derives from it: last use within GenerationStaleAfter).
func (s *KeyStore) TouchKey(ctx context.Context, id int64) {
	_, _ = s.db.ExecContext(ctx, `UPDATE keys SET last_used_at = ? WHERE id = ?`, time.Now().UTC(), id)
}
```

Write `cmd/woca-relay/main.go`:

```go
// woca-relay is the personal-edition remote relay (design
// 2026-09-17-personal-relay-design): a single public-entry binary for a
// VPS. NAT-hidden personal woca instances poll it outbound (bridge side);
// browsers and native clients reach it with a wcr_ rendezvous key (client
// side). It holds no business data — keys, sessions, and an audit log
// only; runs, conversations, and files stay on the bridge.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/projanvil/woca/internal/relay"
	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var dbPath string
	root := &cobra.Command{
		Use:           "woca-relay",
		Short:         "woca personal remote relay (public entry + key authority)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&dbPath, "db", defaultDBPath(), "relay database path (env WOCA_RELAY_DB)")
	root.AddCommand(newKeyCmd(&dbPath))
	return root
}

// defaultDBPath is the operator-conventional relay database location.
func defaultDBPath() string {
	if v := os.Getenv("WOCA_RELAY_DB"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "woca-relay.db"
	}
	return filepath.Join(home, ".woca-relay", "relay.db")
}

// openKeys opens the key/session store for one CLI invocation.
func openKeys(dbPath string) (*relay.KeyStore, error) {
	return relay.OpenKeyStore(dbPath)
}
```

Write `cmd/woca-relay/key.go`:

```go
package main

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/projanvil/woca/internal/relay"
	"github.com/spf13/cobra"
)

// newKeyCmd builds `woca-relay key create|list|revoke` (design §4.3).
func newKeyCmd(dbPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Manage rendezvous keys (create | list | revoke)",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "create <name>",
		Short: "Mint a new wcr_ key (printed once — store it now)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ks, err := openKeys(*dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = ks.Close() }()
			k, raw, err := ks.CreateKey(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			// The full key goes to stdout exactly once (scripts scrape it);
			// the store keeps only its SHA-256 (design §4.1).
			fmt.Printf("%s\n", raw)
			fmt.Fprintf(os.Stderr, "created key %q — the full key above is shown only once\n", k.Name)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List keys (prefix, created, last used, status, bridge online)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ks, err := openKeys(*dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = ks.Close() }()
			keys, err := ks.ListKeys(cmd.Context())
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tPREFIX\tCREATED\tLAST USED\tSTATUS\tBRIDGE")
			for _, k := range keys {
				status := "active"
				if k.RevokedAt != nil {
					status = "revoked"
				}
				lastUsed := "-"
				bridge := "offline"
				if k.LastUsedAt != nil {
					lastUsed = k.LastUsedAt.Format(time.DateTime)
					// Bridge online = tunnel activity within the generation
					// lease window (design §6.2).
					if time.Since(*k.LastUsedAt) < relay.GenerationStaleAfter {
						bridge = "online"
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					k.Name, k.Prefix, k.CreatedAt.Format(time.DateTime), lastUsed, status, bridge)
			}
			return w.Flush()
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "revoke <name>",
		Short: "Revoke a key (the running relay cascades it to both sides)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ks, err := openKeys(*dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = ks.Close() }()
			k, ok, err := ks.RevokeKey(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("unknown or already revoked key %q", args[0])
			}
			// The revocation itself is this row write; the running relay's
			// sweep (serve subcommand) notices it and cancels in-flight
			// polls, deletes sessions, and drains the parking queue.
			fmt.Printf("revoked key %q (%s)\n", k.Name, k.Prefix)
			return nil
		},
	})
	return cmd
}
```

- [ ] **Step 4: Run test to verify it passes**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
go test ./internal/relay/ -count=1
```

Expected: `ok  	github.com/projanvil/woca/internal/relay` (all 3 tests PASS).

Then smoke the CLI:

```sh
go build -o build/woca-relay ./cmd/woca-relay
./build/woca-relay --db /tmp/woca-relay-task1.db key create home-mac
./build/woca-relay --db /tmp/woca-relay-task1.db key list
./build/woca-relay --db /tmp/woca-relay-task1.db key revoke home-mac
./build/woca-relay --db /tmp/woca-relay-task1.db key list
rm -f /tmp/woca-relay-task1.db
```

Expected: `create` prints one `wcr_…` line on stdout; `list` shows `home-mac … active offline` then `revoked offline`; second `revoke` exits non-zero with `unknown or already revoked key "home-mac"`.

- [ ] **Step 5: Commit**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
git add internal/relay/keystore.go internal/relay/keystore_test.go cmd/woca-relay/main.go cmd/woca-relay/key.go
git commit -m "relay: keystore, sqlite schema, and woca-relay key CLI"
```

---

### Task 2: Session service + auth HTTP routes + rate limiter

**Files:**
- Create: internal/relay/session.go
- Create: internal/relay/ratelimit.go
- Create: internal/relay/errors.go
- Create: internal/relay/authhttp.go
- Test: internal/relay/session_test.go
- Test: internal/relay/ratelimit_test.go
- Test: internal/relay/authhttp_test.go

**Interfaces:**
- Consumes (Task 1): `KeyStore` (+ its `db` field, same package), `Key`, `ErrKeyUnknown`, `ErrKeyRevoked`, `HashToken`, `openTestKeyStore(t)`.
- Produces (relied on by Tasks 3/5/6):
  - `const SessionCookieName = "woca_relay_session"`, `const SessionIdleTTL = 7*24h`, `const SessionMaxAge = 30*24h`
  - `var ErrSessionUnknown error`
  - `type Session struct { KeyID int64; KeyName string; UserID string; CreatedAt, TouchedAt time.Time }`
  - `func (s *KeyStore) CreateSession(ctx, keyID int64) (token string, expiresAt time.Time, err error)`
  - `func (s *KeyStore) ResolveSession(ctx, token string) (Session, error)`
  - `func (s *KeyStore) DeleteSession(ctx, token string) error`, `func (s *KeyStore) DeleteSessionsForKey(ctx, keyID int64) error`
  - `func NewRateLimiter(perMinute float64, maxEntries int) *RateLimiter`, `func (l *RateLimiter) Allow(key string) bool`
  - `const CodeUnauthorized = "unauthorized"`, `const CodeRevoked = "revoked"`, `const CodeBridgeOffline = "bridge_offline"`
  - `func WriteError(w, status, code, message)`, `func writeJSON(w, status, v any)`
  - `func NewAuthService(keys *KeyStore, behindProxy bool, log zerolog.Logger) *AuthService`
  - `func (a *AuthService) HandleLogin/HandleLogout/HandleMe(w, r)`, `func (a *AuthService) SessionFromRequest(r) (Session, error)`, `func (a *AuthService) ClientIP(r) string`, `func (a *AuthService) SecureCookie(r) bool`
  - `func SessionToken(r *http.Request) string`, `func writeAuthError(w, err)`
  - Test helpers (same package): `testLogger() zerolog.Logger`, `newTestAuth(t) (*AuthService, rawKey string)`

- [ ] **Step 1: Write the failing tests**

Write `internal/relay/session_test.go`:

```go
package relay

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSessionLifecycle(t *testing.T) {
	ks := openTestKeyStore(t)
	ctx := context.Background()
	k, _, err := ks.CreateKey(ctx, "sess")
	if err != nil {
		t.Fatal(err)
	}
	token, expiresAt, err := ks.CreateSession(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("empty session token")
	}
	if until := time.Until(expiresAt); until < SessionMaxAge-time.Minute || until > SessionMaxAge {
		t.Fatalf("expiresAt %v out, want ~30d", until)
	}
	sess, err := ks.ResolveSession(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if sess.KeyID != k.ID || sess.KeyName != "sess" || sess.UserID != k.UserID() {
		t.Fatalf("session %+v (key %+v)", sess, k)
	}
}

func TestSessionSlidingAndAbsoluteExpiry(t *testing.T) {
	ks := openTestKeyStore(t)
	ctx := context.Background()
	k, _, err := ks.CreateKey(ctx, "ttl")
	if err != nil {
		t.Fatal(err)
	}
	age := func(token string, created, touched time.Duration) {
		t.Helper()
		if _, err := ks.db.ExecContext(ctx,
			`UPDATE sessions SET created_at = ?, touched_at = ? WHERE token_hash = ?`,
			time.Now().UTC().Add(-created), time.Now().UTC().Add(-touched), HashToken(token)); err != nil {
			t.Fatal(err)
		}
	}

	// Idle beyond the 7d sliding window: dead even though 30d unspent.
	token, _, err := ks.CreateSession(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	age(token, 2*24*time.Hour, 8*24*time.Hour)
	if _, err := ks.ResolveSession(ctx, token); !errors.Is(err, ErrSessionUnknown) {
		t.Fatalf("idle session: %v, want ErrSessionUnknown", err)
	}

	// Past the 30d absolute cap with a fresh touch: still dead (OWASP —
	// sliding expiry must have an absolute ceiling, design §4.4).
	token2, _, err := ks.CreateSession(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	age(token2, 31*24*time.Hour, time.Hour)
	if _, err := ks.ResolveSession(ctx, token2); !errors.Is(err, ErrSessionUnknown) {
		t.Fatalf("absolute-expired session: %v, want ErrSessionUnknown", err)
	}

	// Inside both windows: alive, and resolving slides the touch forward.
	token3, _, err := ks.CreateSession(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	age(token3, 6*24*time.Hour, 6*24*time.Hour)
	sess, err := ks.ResolveSession(ctx, token3)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(sess.TouchedAt) > time.Minute {
		t.Fatalf("sliding touch not refreshed: %v ago", time.Since(sess.TouchedAt))
	}
}

func TestSessionDeleteAndRevokeDelete(t *testing.T) {
	ks := openTestKeyStore(t)
	ctx := context.Background()
	k, _, err := ks.CreateKey(ctx, "rev")
	if err != nil {
		t.Fatal(err)
	}

	// Logout is a server-side delete (design §4.4).
	token, _, err := ks.CreateSession(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.DeleteSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := ks.ResolveSession(ctx, token); !errors.Is(err, ErrSessionUnknown) {
		t.Fatalf("deleted session: %v, want ErrSessionUnknown", err)
	}

	// DeleteSessionsForKey (maintenance/logout-everywhere API — NOT the
	// revocation path, which tombstones; Task 6) clears the whole key.
	t1, _, err := ks.CreateSession(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	t2, _, err := ks.CreateSession(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.DeleteSessionsForKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{t1, t2} {
		if _, err := ks.ResolveSession(ctx, tok); !errors.Is(err, ErrSessionUnknown) {
			t.Fatalf("post-revoke session: %v, want ErrSessionUnknown", err)
		}
	}
}
```

Write `internal/relay/ratelimit_test.go`:

```go
package relay

import (
	"fmt"
	"testing"
	"time"
)

func TestRateLimiterWindow(t *testing.T) {
	now := time.Now()
	l := NewRateLimiter(10, 100)
	l.now = func() time.Time { return now }

	for i := 0; i < 10; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("attempt %d denied inside the 10/min window", i+1)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("11th attempt inside the window allowed")
	}
	// Buckets are per key: another IP has its own allowance.
	if !l.Allow("5.6.7.8") {
		t.Fatal("per-IP isolation broken")
	}
	now = now.Add(61 * time.Second)
	if !l.Allow("1.2.3.4") {
		t.Fatal("window did not refill")
	}
}

func TestRateLimiterBounded(t *testing.T) {
	l := NewRateLimiter(10, 50)
	// A spray of distinct source IPs must not grow the table without
	// bound (memory-DoS posture, design §4.4).
	for i := 0; i < 500; i++ {
		l.Allow(fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}
	l.mu.Lock()
	n := len(l.entries)
	l.mu.Unlock()
	if n > 50 {
		t.Fatalf("entries %d exceed maxEntries 50", n)
	}
}
```

Write `internal/relay/authhttp_test.go`:

```go
package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func testLogger() zerolog.Logger { return zerolog.Nop() }

func newTestAuth(t *testing.T) (*AuthService, string) {
	t.Helper()
	ks := openTestKeyStore(t)
	_, raw, err := ks.CreateKey(t.Context(), "home-mac")
	if err != nil {
		t.Fatal(err)
	}
	return NewAuthService(ks, false, testLogger()), raw
}

func loginFor(t *testing.T, a *AuthService, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"api_key":"`+apiKey+`"}`))
	rec := httptest.NewRecorder()
	a.HandleLogin(rec, req)
	return rec
}

func TestLoginCookieAndBodyToken(t *testing.T) {
	a, raw := newTestAuth(t)
	rec := loginFor(t, a, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("no %s cookie", SessionCookieName)
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie flags: %+v (want HttpOnly + SameSite=Lax)", cookie)
	}
	// The body token is the native client's Bearer credential (design
	// §3.3 — cookie AND body token, the edgegw precedent).
	var body struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
		UserID    string `json:"user_id"`
		Remark    string `json:"remark"`
		Role      string `json:"role"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Token == "" || body.ExpiresAt == 0 || body.UserID == "" ||
		body.Remark != "home-mac" || body.Role != "admin" {
		t.Fatalf("login body: %+v", body)
	}

	// Bearer channel (native).
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+body.Token)
	meRec := httptest.NewRecorder()
	a.HandleMe(meRec, req)
	if meRec.Code != http.StatusOK ||
		!strings.Contains(meRec.Body.String(), `"remark":"home-mac"`) ||
		!strings.Contains(meRec.Body.String(), `"role":"admin"`) {
		t.Fatalf("me via bearer: %d %s", meRec.Code, meRec.Body)
	}

	// Cookie channel (browser EventSource cannot set headers).
	req = httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(cookie)
	meRec = httptest.NewRecorder()
	a.HandleMe(meRec, req)
	if meRec.Code != http.StatusOK {
		t.Fatalf("me via cookie: %d %s", meRec.Code, meRec.Body)
	}
}

func TestLoginFailureShapes(t *testing.T) {
	a, raw := newTestAuth(t)
	if rec := loginFor(t, a, "wcr_"+strings.Repeat("A", 43)); rec.Code != http.StatusUnauthorized ||
		!strings.Contains(rec.Body.String(), `"code":"unauthorized"`) {
		t.Fatalf("unknown key: %d %s", rec.Code, rec.Body)
	}
	// A revoked key says so with the frontend contract code — the SPA's
	// revoked notice keys off exactly "revoked" (design §2).
	if _, ok, err := a.keys.RevokeKey(t.Context(), "home-mac"); err != nil || !ok {
		t.Fatal("revoke setup failed")
	}
	rec := loginFor(t, a, raw)
	if rec.Code != http.StatusUnauthorized ||
		!strings.Contains(rec.Body.String(), `"code":"revoked"`) {
		t.Fatalf("revoked login: %d %s (must carry code revoked)", rec.Code, rec.Body)
	}
}

func TestLogoutDualChannel(t *testing.T) {
	a, raw := newTestAuth(t)
	rec := loginFor(t, a, raw)
	var body struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// Logout accepts Bearer (native) — and cookie (browser) — design §3.3.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+body.Token)
	outRec := httptest.NewRecorder()
	a.HandleLogout(outRec, req)
	if outRec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", outRec.Code)
	}
	if _, err := a.SessionFromRequest(req); err == nil {
		t.Fatal("session survived logout (server-side delete required)")
	}
}

func TestSecureCookieProxyModel(t *testing.T) {
	a, _ := newTestAuth(t)
	r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	// Plain HTTP without --behind-proxy (dev loopback): not Secure.
	if a.SecureCookie(r) {
		t.Fatal("direct plain HTTP must not mark the cookie Secure")
	}
	// A client-forged X-Forwarded-Proto is NOT trusted by default
	// (design §7.3 — the cookie-downgrade vector).
	r.Header.Set("X-Forwarded-Proto", "https")
	if a.SecureCookie(r) {
		t.Fatal("X-Forwarded-Proto trusted without --behind-proxy")
	}
	// Only an explicit --behind-proxy trusts the front's statement.
	a.behindProxy = true
	if !a.SecureCookie(r) {
		t.Fatal("--behind-proxy must trust X-Forwarded-Proto: https")
	}
}

func TestLoginRateLimit(t *testing.T) {
	ks := openTestKeyStore(t)
	a := NewAuthService(ks, false, testLogger())
	// All attempts from one source IP (httptest.NewRequest default).
	for i := 0; i < 10; i++ {
		if rec := loginFor(t, a, "bad"); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d rate-limited inside the allowance", i+1)
		}
	}
	if rec := loginFor(t, a, "bad"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("11th attempt: %d, want 429", rec.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
go test ./internal/relay/ -run 'TestSession|TestRateLimiter|TestLogin|TestLogout|TestSecureCookie' -count=1
```

Expected failure: compile errors — `undefined: SessionIdleTTL`, `undefined: SessionMaxAge`, `undefined: ErrSessionUnknown`, `undefined: NewRateLimiter`, `undefined: NewAuthService`, `undefined: SessionCookieName`.

- [ ] **Step 3: Write minimal implementation**

Write `internal/relay/session.go`:

```go
package relay

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Session expiry (design §4.4 — OWASP: sliding expiry needs an absolute
// cap, otherwise a stolen credential renews forever).
const (
	SessionIdleTTL = 7 * 24 * time.Hour
	SessionMaxAge  = 30 * 24 * time.Hour
)

// ErrSessionUnknown rejects absent, expired, or deleted sessions.
var ErrSessionUnknown = errors.New("relay: unknown or expired session")

// Session is a resolved client session (web cookie and native Bearer
// share the sessions table, design §3.3).
type Session struct {
	KeyID     int64
	KeyName   string
	UserID    string
	CreatedAt time.Time
	TouchedAt time.Time
}

// CreateSession mints an opaque 128-bit session token (design §4.4); the
// store keeps only its SHA-256. expiresAt is the absolute cap.
func (s *KeyStore) CreateSession(ctx context.Context, keyID int64) (string, time.Time, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, fmt.Errorf("relay: mint session: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, key_id, created_at, touched_at) VALUES (?, ?, ?, ?)`,
		HashToken(token), keyID, now, now); err != nil {
		return "", time.Time{}, fmt.Errorf("relay: create session: %w", err)
	}
	return token, now.Add(SessionMaxAge), nil
}

// ResolveSession validates token and slides its idle expiry forward.
// Errors: ErrSessionUnknown (absent/expired), ErrKeyRevoked (owning key
// revoked while the session row still exists).
func (s *KeyStore) ResolveSession(ctx context.Context, token string) (Session, error) {
	var sess Session
	var keyHash []byte
	var revokedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT s.key_id, s.created_at, s.touched_at, k.name, k.key_hash, k.revoked_at
		FROM sessions s JOIN keys k ON k.id = s.key_id
		WHERE s.token_hash = ?`, HashToken(token)).
		Scan(&sess.KeyID, &sess.CreatedAt, &sess.TouchedAt, &sess.KeyName, &keyHash, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrSessionUnknown
	}
	if err != nil {
		return Session{}, fmt.Errorf("relay: resolve session: %w", err)
	}
	if revokedAt.Valid {
		return Session{}, ErrKeyRevoked
	}
	now := time.Now().UTC()
	if now.Sub(sess.TouchedAt) > SessionIdleTTL || now.Sub(sess.CreatedAt) > SessionMaxAge {
		_ = s.DeleteSession(ctx, token)
		return Session{}, ErrSessionUnknown
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET touched_at = ? WHERE token_hash = ?`, now, HashToken(token)); err != nil {
		return Session{}, fmt.Errorf("relay: touch session: %w", err)
	}
	sess.TouchedAt = now
	sess.UserID = hex.EncodeToString(keyHash)[:8]
	return sess, nil
}

// DeleteSession removes one session (logout is a server-side delete —
// clearing the cookie alone does not count, design §4.4).
func (s *KeyStore) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, HashToken(token))
	return err
}

// DeleteSessionsForKey removes every session derived from keyID. Kept
// as store API for maintenance paths (e.g. an operator-initiated
// logout-everywhere); the revocation cascade deliberately does NOT call
// it — revocation tombstones sessions so ResolveSession's keys join can
// still answer ErrKeyRevoked (code "revoked") instead of the
// indistinguishable-from-logged-out ErrSessionUnknown (Task 6).
func (s *KeyStore) DeleteSessionsForKey(ctx context.Context, keyID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE key_id = ?`, keyID)
	return err
}
```

Write `internal/relay/ratelimit.go`:

```go
package relay

import (
	"sync"
	"time"
)

// RateLimiter is a bounded per-key token bucket written against the
// standard library only (design §4.4: golang.org/x/time/rate is NOT in
// the personal dependency baseline and would fail check-deps). The entry
// map is capped and lazily evicted so a spray of source IPs cannot grow
// memory without bound.
type RateLimiter struct {
	mu         sync.Mutex
	rate       float64 // tokens per second
	burst      float64
	maxEntries int
	now        func() time.Time
	entries    map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter allows perMinute tokens per minute per key (burst equal
// to perMinute) and keeps at most maxEntries live buckets.
func NewRateLimiter(perMinute float64, maxEntries int) *RateLimiter {
	return &RateLimiter{
		rate:       perMinute / 60,
		burst:      perMinute,
		maxEntries: maxEntries,
		now:        time.Now,
		entries:    map[string]*bucket{},
	}
}

// Allow consumes one token for key, reporting whether the call is within
// the limit.
func (l *RateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if _, ok := l.entries[key]; !ok && len(l.entries) >= l.maxEntries {
		l.evictLocked(now)
	}
	b, ok := l.entries[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.entries[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// evictLocked drops buckets idle for more than ten minutes; when the map
// is still at capacity it trims arbitrary survivors down to 90% (map
// order is random — under a flood the cost is a temporarily wider limit
// for evicted IPs, the right trade under a DoS posture).
func (l *RateLimiter) evictLocked(now time.Time) {
	for k, b := range l.entries {
		if now.Sub(b.last) > 10*time.Minute {
			delete(l.entries, k)
		}
	}
	floor := l.maxEntries * 9 / 10
	for k := range l.entries {
		if len(l.entries) <= floor {
			return
		}
		delete(l.entries, k)
	}
}
```

Write `internal/relay/errors.go`:

```go
package relay

import (
	"encoding/json"
	"net/http"
)

// Wire error codes (design §2/§9). "revoked" is a frontend contract — the
// SPA and native clients program against exactly this string (never
// "key_revoked"); internal/platform/authn.CodeRevoked is the personal
// binary's own copy of the same vocabulary.
const (
	CodeUnauthorized  = "unauthorized"
	CodeRevoked       = "revoked"
	CodeBridgeOffline = "bridge_offline"
)

// WriteError emits the relay's JSON error envelope.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "error": message})
}

// writeJSON is the success-path counterpart.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
```

Write `internal/relay/authhttp.go`:

```go
package relay

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/rs/zerolog"
)

// SessionCookieName is the client-session cookie (design §4.4).
const SessionCookieName = "woca_relay_session"

// loginAttemptsPerMinute bounds /api/auth/login per source IP (design §4.4).
const loginAttemptsPerMinute = 10

// AuthService owns the client-auth surface: login (rate-limited), logout,
// me, and session resolution for the forwarding plane. It is the relay's
// ONLY authentication line of defense (design §7.1).
type AuthService struct {
	keys        *KeyStore
	limiter     *RateLimiter
	behindProxy bool
	log         zerolog.Logger
}

// NewAuthService builds the auth surface; behindProxy is the explicit
// X-Forwarded-* trust switch (design §7.3).
func NewAuthService(keys *KeyStore, behindProxy bool, log zerolog.Logger) *AuthService {
	return &AuthService{
		keys:        keys,
		limiter:     NewRateLimiter(loginAttemptsPerMinute, 1024),
		behindProxy: behindProxy,
		log:         log,
	}
}

type loginRequest struct {
	APIKey string `json:"api_key"`
}

// HandleLogin implements POST /api/auth/login (design §3.3/§4.4): the SPA
// posts {api_key} and receives the HttpOnly session cookie plus — in the
// same response body — the opaque token native clients present as Bearer
// (the edgegw precedent: cookie AND body token, edgegw/auth.go:254-259
// semantics).
func (a *AuthService) HandleLogin(w http.ResponseWriter, r *http.Request) {
	ip := a.ClientIP(r)
	if !a.limiter.Allow(ip) {
		a.log.Warn().Str("ip", ip).Msg("relay: login rate limit exceeded")
		WriteError(w, http.StatusTooManyRequests, CodeUnauthorized, "too many attempts; try again later")
		return
	}
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeUnauthorized, "invalid request")
		return
	}
	key, err := a.keys.LookupKey(r.Context(), req.APIKey)
	if errors.Is(err, ErrKeyRevoked) {
		// The SPA's revoked notice keys off exactly this code (design §2).
		WriteError(w, http.StatusUnauthorized, CodeRevoked, "key revoked")
		return
	}
	if err != nil {
		a.log.Warn().Str("ip", ip).Msg("relay: login failed")
		WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "invalid key")
		return
	}
	token, expiresAt, err := a.keys.CreateSession(r.Context(), key.ID)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeUnauthorized, "session unavailable")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.SecureCookie(r),
		MaxAge:   int(SessionMaxAge.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"expires_at": expiresAt.UnixMilli(),
		"user_id":    key.UserID(),
		"remark":     key.Name,
		"role":       "admin",
	})
}

// HandleLogout implements POST /api/auth/logout: cookie OR Bearer (design
// §3.3); logout is a server-side session delete.
func (a *AuthService) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if token := SessionToken(r); token != "" {
		_ = a.keys.DeleteSession(r.Context(), token)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

// HandleMe implements GET /api/auth/me with the SPA's Me shape (design
// §3.3): user_id is the key's hash-derived stable id, remark the key
// name, role fixed admin.
func (a *AuthService) HandleMe(w http.ResponseWriter, r *http.Request) {
	sess, err := a.SessionFromRequest(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id": sess.UserID,
		"remark":  sess.KeyName,
		"role":    "admin",
	})
}

// SessionToken extracts the session credential: cookie first (browser
// EventSource cannot set headers), then Bearer (native clients).
func SessionToken(r *http.Request) string {
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// SessionFromRequest resolves the request's session to its key. Errors:
// ErrSessionUnknown (401 unauthorized), ErrKeyRevoked (401 revoked).
func (a *AuthService) SessionFromRequest(r *http.Request) (Session, error) {
	token := SessionToken(r)
	if token == "" {
		return Session{}, ErrSessionUnknown
	}
	return a.keys.ResolveSession(r.Context(), token)
}

// writeAuthError maps session-resolution failures onto the wire.
func writeAuthError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrKeyRevoked) {
		WriteError(w, http.StatusUnauthorized, CodeRevoked, "key revoked")
		return
	}
	WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "unauthorized")
}

// ClientIP is the rate-limit/audit source address (design §7.3): the TCP
// peer by default; X-Forwarded-For only under explicit --behind-proxy.
func (a *AuthService) ClientIP(r *http.Request) string {
	if a.behindProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			if ip, _, _ := strings.Cut(fwd, ","); strings.TrimSpace(ip) != "" {
				return strings.TrimSpace(ip)
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// SecureCookie decides the session cookie's Secure flag (design §7.3):
// direct TLS is always secure; X-Forwarded-Proto is trusted only under
// --behind-proxy — otherwise a client could forge proto:http and trick
// the relay into issuing a cookie that also travels plaintext.
func (a *AuthService) SecureCookie(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return a.behindProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
```

- [ ] **Step 4: Run tests to verify they pass**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
go test ./internal/relay/ -count=1
```

Expected: `ok  	github.com/projanvil/woca/internal/relay` — all Task 1 + Task 2 tests PASS (including `TestLoginRateLimit` at exactly the 10/11 boundary).

- [ ] **Step 5: Commit**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
git add internal/relay/session.go internal/relay/ratelimit.go internal/relay/errors.go internal/relay/authhttp.go internal/relay/session_test.go internal/relay/ratelimit_test.go internal/relay/authhttp_test.go
git commit -m "relay: session service, auth routes, stdlib rate limiter"
```

---

### Task 3: `woca-relay serve` skeleton (bind policy, proxy trust, SPA, health)

**Files:**
- Create: internal/relay/serve.go
- Create: cmd/woca-relay/serve.go
- Modify: cmd/woca-relay/main.go:41-44 (register the serve subcommand)
- Test: internal/relay/serve_test.go

**Interfaces:**
- Consumes (Tasks 1-2): `KeyStore`, `NewAuthService`, `AuthService.HandleLogin/HandleLogout/HandleMe`, `testLogger()`, `openTestKeyStore(t)`; repo: `webfx.SPA(root fs.FS) http.Handler` (internal/webfx/spa.go:15 — `/api/` misses 404, never fall back), `webfx.New(webfx.Options{Logger, Grace, ReadHeaderTimeout})`, `(*webfx.Router).UseDefaults()`, `(*webfx.Router).Handle(pattern, h)`, `(*webfx.Router).Serve(ctx, ln)` (internal/webfx/lifecycle.go:64), `webdist.DistFS() fs.FS` (web/embed.go:32), `logging.ParseLevel`/`logging.NewConsole` (internal/logging/logging.go:15,46), `isPublicBind` shape from cmd/woca-edge/main.go:167-179.
- Produces (relied on by Tasks 5/6):
  - `type Relay struct` with fields `keys *KeyStore`, `auth *AuthService`, `spa http.Handler`, `behindProxy bool`, `log zerolog.Logger`, `park *ParkingQueue`, `gens *GenerationRegistry`, `tunnel *TunnelService`, `audit *AuditLog`
  - `func NewRelay(keys *KeyStore, spaFS fs.FS, behindProxy bool, log zerolog.Logger) *Relay`
  - `func (rl *Relay) Handler() http.Handler`
  - `func CheckBind(addr string, tlsOn, insecure, behindProxy bool, log zerolog.Logger) error`
  - Test helper (same package): `testSPA() fstest.MapFS`
  - CLI: `woca-relay serve --addr --tls-cert --tls-key --insecure --behind-proxy --log-level`

- [ ] **Step 1: Write the failing test**

Write `internal/relay/serve_test.go`:

```go
package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testSPA() fstest.MapFS {
	return fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>woca</title>app")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	}
}

func TestCheckBindFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name          string
		addr          string
		tls, dev, prx bool
		wantErr       bool
	}{
		{"public bind without TLS refuses", ":443", false, false, false, true},
		{"public bind with TLS passes", ":443", true, false, false, false},
		{"loopback plain HTTP passes", "127.0.0.1:8443", false, false, false, false},
		{"localhost plain HTTP passes", "localhost:8443", false, false, false, false},
		{"public insecure dev opt-in passes", "0.0.0.0:8080", false, true, false, false},
		{"public behind-proxy warns but passes", ":8080", false, false, true, false},
		{"wildcard IPv6 without TLS refuses", "[::]:443", false, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckBind(tc.addr, tc.tls, tc.dev, tc.prx, testLogger())
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckBind(%q, tls=%v, dev=%v, proxy=%v) = %v, wantErr %v",
					tc.addr, tc.tls, tc.dev, tc.prx, err, tc.wantErr)
			}
		})
	}
}

func TestRelaySkeletonSurface(t *testing.T) {
	ks := openTestKeyStore(t)
	_, raw, err := ks.CreateKey(t.Context(), "home-mac")
	if err != nil {
		t.Fatal(err)
	}
	rl := NewRelay(ks, testSPA(), false, testLogger())
	ts := httptest.NewServer(rl.Handler())
	t.Cleanup(ts.Close)

	get := func(path string) (int, http.Header, string) {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(body)
	}

	// Anonymous whitelist (design §7.2): health + SPA + login only.
	if code, _, body := get("/api/health"); code != http.StatusOK ||
		!strings.Contains(body, `"ok":true`) {
		t.Fatalf("health: %d %s", code, body)
	}
	if code, hdr, body := get("/"); code != http.StatusOK ||
		!strings.Contains(hdr.Get("Content-Type"), "text/html") ||
		!strings.Contains(body, "app") {
		t.Fatalf("spa root: %d %s", code, body)
	}
	// Client-side routes fall back to the app shell.
	if code, _, body := get("/chat/session/abc"); code != http.StatusOK ||
		!strings.Contains(body, "app") {
		t.Fatalf("spa fallback: %d %s", code, body)
	}
	if code, _, body := get("/assets/app.js"); code != http.StatusOK ||
		!strings.Contains(body, "console.log") {
		t.Fatalf("spa asset: %d %s", code, body)
	}
	// An /api/ miss is a 404, never the app shell (webfx.SPA semantics —
	// exactly what the relay wants).
	if code, _, _ := get("/api/models"); code != http.StatusNotFound {
		t.Fatalf("anonymous /api/models: %d, want 404 in the skeleton", code)
	}
	// Login is reachable anonymously; me/logout demand a session.
	resp, err := http.Post(ts.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"api_key":"`+raw+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	if code, _, body := get("/api/auth/me"); code != http.StatusUnauthorized ||
		!strings.Contains(body, `"code":"unauthorized"`) {
		t.Fatalf("anonymous me: %d %s", code, body)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
go test ./internal/relay/ -run 'TestCheckBind|TestRelaySkeleton' -count=1
```

Expected failure: compile errors — `undefined: CheckBind`, `undefined: NewRelay`.

- [ ] **Step 3: Write minimal implementation**

Write `internal/relay/serve.go`:

```go
package relay

import (
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/webfx"
)

// Relay is the public surface assembly: auth routes, the SPA, the health
// probe, and — once the tunnel task attaches it — the forwarding plane.
type Relay struct {
	keys        *KeyStore
	auth        *AuthService
	spa         http.Handler
	behindProxy bool
	log         zerolog.Logger

	park   *ParkingQueue       // attached by the tunnel task
	gens   *GenerationRegistry // attached by the tunnel task
	tunnel *TunnelService      // attached by the tunnel task
	audit  *AuditLog           // attached by the revocation task
}

// NewRelay builds the surface skeleton (auth + SPA + health). The tunnel
// and revocation planes attach in later tasks; until then /api/ misses
// 404 through the SPA handler (webfx.SPA semantics).
func NewRelay(keys *KeyStore, spaFS fs.FS, behindProxy bool, log zerolog.Logger) *Relay {
	return &Relay{
		keys:        keys,
		auth:        NewAuthService(keys, behindProxy, log),
		spa:         webfx.SPA(spaFS),
		behindProxy: behindProxy,
		log:         log,
	}
}

// Handler is the full public surface. Anonymous reach is exactly the
// design §7.2 whitelist: SPA assets, /api/health, /api/auth/login.
// /tunnel/ endpoints enforce Bearer-key + generation auth inside their
// own handlers; every other /api/ path requires a session once the
// forwarding plane is attached.
func (rl *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "woca-relay"})
	})
	mux.HandleFunc("POST /api/auth/login", rl.auth.HandleLogin)
	mux.HandleFunc("POST /api/auth/logout", rl.auth.HandleLogout)
	mux.HandleFunc("GET /api/auth/me", rl.auth.HandleMe)
	mux.HandleFunc("/", rl.route)
	return mux
}

// route sends everything the mux did not claim to the SPA: unknown /api/
// paths 404 there (never the app shell), unknown page paths render the
// app shell. The tunnel task extends this for authenticated forwarding.
func (rl *Relay) route(w http.ResponseWriter, r *http.Request) {
	rl.spa.ServeHTTP(w, r)
}

// CheckBind enforces the design §7.2 fail-closed red line: a public bind
// without TLS refuses to start unless --insecure was passed explicitly
// (development only). --behind-proxy declares a trusted TLS-terminating
// front (Caddy / CF Tunnel / Tailscale Funnel); the relay then serves
// plain HTTP and should bind loopback or an internal interface — a public
// behind-proxy bind logs a warning instead of failing, because some
// fronts legitimately reach the relay over a private-looking address.
func CheckBind(addr string, tlsOn, insecure, behindProxy bool, log zerolog.Logger) error {
	if tlsOn || !isPublicBind(addr) {
		return nil
	}
	if insecure {
		log.Warn().Str("addr", addr).Msg("relay: plain HTTP on a public bind (--insecure: development only)")
		return nil
	}
	if behindProxy {
		log.Warn().Str("addr", addr).Msg("relay: --behind-proxy on a public bind; the TLS front should reach the relay over loopback or an internal interface")
		return nil
	}
	return fmt.Errorf("relay: refusing to serve plain HTTP on non-loopback bind %s "+
		"(set --tls-cert/--tls-key, --behind-proxy behind a TLS front, or --insecure for development)", addr)
}

// isPublicBind reports whether the bind address reaches beyond loopback
// (same rule as woca-edge's entry-point guard, cmd/woca-edge/main.go:167).
func isPublicBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return true // ":port" binds every interface
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return !ip.IsLoopback()
	}
	return !strings.EqualFold(host, "localhost")
}
```

Write `cmd/woca-relay/serve.go`:

```go
package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/projanvil/woca/internal/logging"
	"github.com/projanvil/woca/internal/relay"
	"github.com/projanvil/woca/internal/webfx"
	webdist "github.com/projanvil/woca/web"
	"github.com/spf13/cobra"
)

// newServeCmd builds `woca-relay serve`: the public listener (client SPA
// + auth + tunnel) with the fail-closed bind policy (design §7.2/§7.3).
func newServeCmd(dbPath *string) *cobra.Command {
	var logLevel, addr, tlsCert, tlsKey string
	var insecure, behindProxy bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the public relay listener (SPA + auth + tunnel)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			level, err := logging.ParseLevel(logLevel)
			if err != nil {
				return err
			}
			log := logging.NewConsole(os.Stderr, level)
			keys, err := openKeys(*dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = keys.Close() }()

			tlsOn := tlsCert != "" && tlsKey != ""
			if err := relay.CheckBind(addr, tlsOn, insecure, behindProxy, log); err != nil {
				return err
			}
			rl := relay.NewRelay(keys, webdist.DistFS(), behindProxy, log)

			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return fmt.Errorf("listen %s: %w", addr, err)
			}
			scheme := "http"
			if tlsOn {
				cert, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
				if err != nil {
					_ = ln.Close()
					return fmt.Errorf("tls: load keypair: %w", err)
				}
				ln = tls.NewListener(ln, &tls.Config{
					MinVersion:   tls.VersionTLS12,
					Certificates: []tls.Certificate{cert},
				})
				scheme = "https"
			}
			// webfx owns the surface: request id + request log + recovery.
			// The 40s grace matches the edge posture: parked polls wait up
			// to 25s and SSE relays stream as long as the bridge answers.
			router := webfx.New(webfx.Options{
				Logger:            log,
				Grace:             40 * time.Second,
				ReadHeaderTimeout: 15 * time.Second,
			})
			router.UseDefaults()
			router.Handle("/", rl.Handler())

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			fmt.Fprintf(os.Stderr, "woca-relay: %s://%s\n", scheme, ln.Addr())
			return router.Serve(ctx, ln)
		},
	}
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "debug | info | warn | error")
	cmd.Flags().StringVar(&addr, "addr", ":443", "public listen address (env WOCA_RELAY_ADDR)")
	cmd.Flags().StringVar(&tlsCert, "tls-cert", "", "TLS certificate for the public listener (env WOCA_TLS_CERT)")
	cmd.Flags().StringVar(&tlsKey, "tls-key", "", "TLS private key for the public listener (env WOCA_TLS_KEY)")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "allow plain HTTP on a public bind (development only)")
	cmd.Flags().BoolVar(&behindProxy, "behind-proxy", false, "trust X-Forwarded-* from a TLS-terminating front (bind loopback/internal)")
	for _, env := range []struct{ flag, key string }{
		{"addr", "WOCA_RELAY_ADDR"},
		{"tls-cert", "WOCA_TLS_CERT"},
		{"tls-key", "WOCA_TLS_KEY"},
	} {
		if v := os.Getenv(env.key); v != "" {
			_ = cmd.Flags().Set(env.flag, v)
		}
	}
	return cmd
}
```

Modify `cmd/woca-relay/main.go` — inside `newRootCmd`, after the `newKeyCmd` registration, add the serve subcommand:

```go
	root.AddCommand(newKeyCmd(&dbPath))
	root.AddCommand(newServeCmd(&dbPath))
	return root
```

- [ ] **Step 4: Run test to verify it passes**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
go test ./internal/relay/ -count=1
go build ./cmd/woca-relay
```

Expected: `ok  	github.com/projanvil/woca/internal/relay` and a clean build.

Smoke the bind policy and the skeleton surface:

```sh
go build -o build/woca-relay ./cmd/woca-relay
# Fail-closed: public bind without TLS must refuse.
./build/woca-relay --db /tmp/woca-relay-t3.db serve --addr :8443 2>&1 | head -2
# Dev loopback works.
./build/woca-relay --db /tmp/woca-relay-t3.db serve --addr 127.0.0.1:18443 --log-level warn &
RELAY_PID=$!; sleep 1
curl -s http://127.0.0.1:18443/api/health
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18443/api/models   # 404
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18443/             # 200
kill $RELAY_PID; rm -f /tmp/woca-relay-t3.db
```

Expected: the first command exits non-zero with `refusing to serve plain HTTP on non-loopback bind :8443`; health prints `{"ok":true,"service":"woca-relay"}`; the two status codes are `404` and `200`.

- [ ] **Step 5: Commit**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
git add internal/relay/serve.go internal/relay/serve_test.go cmd/woca-relay/serve.go cmd/woca-relay/main.go
git commit -m "relay: serve skeleton with fail-closed bind policy and SPA surface"
```

---

### Task 4: Parking queue + generation registry

**Files:**
- Create: internal/relay/park.go
- Create: internal/relay/generation.go
- Test: internal/relay/park_test.go
- Test: internal/relay/generation_test.go

**Interfaces:**
- Consumes (Task 1): `GenerationStaleAfter`; (Task 2) nothing directly.
- Produces (relied on by Tasks 5/6):
  - `const ParkCapacity = 64`
  - `var ErrQueueFull, ErrTakeTimeout, ErrUnknownParkedID, ErrClientGone, ErrRevoked error`
  - `type ParkedRequest struct { ID, Method, Path, Query, ClientIP string; Headers map[string][]string; Body []byte }`
  - `type ParkedResponse struct { Status int; Headers http.Header; Body io.ReadCloser }`
  - `func NewParkingQueue(capacity int) *ParkingQueue`
  - `func (q *ParkingQueue) Park(ctx, keyID int64, req ParkedRequest) (resp *ParkedResponse, done func(), failCause <-chan error, err error)`
  - `func (q *ParkingQueue) Take(ctx, keyID int64, generation string, wait time.Duration) (*ParkedRequest, error)`
  - `func (q *ParkingQueue) Complete(keyID int64, generation, id string, resp *ParkedResponse) (<-chan struct{}, error)`
  - `func (q *ParkingQueue) FailAll(keyID int64, err error)`, `func (q *ParkingQueue) Len(keyID int64) int`
  - `var ErrGenerationConflict error`
  - `func NewGenerationRegistry() *GenerationRegistry` (field `now func() time.Time` swappable in tests)
  - `func (r *GenerationRegistry) Claim(keyID int64, gen string) (adopted, evicted bool, err error)`
  - `func (r *GenerationRegistry) Current(keyID int64, gen string) bool`, `func (r *GenerationRegistry) Online(keyID int64) bool`
  - `func (r *GenerationRegistry) RegisterPoll(keyID int64, gen string, cancel context.CancelCauseFunc) (unregister func(), ok bool)`
  - `func (r *GenerationRegistry) CancelPolls(keyID int64, cause error)`, `func (r *GenerationRegistry) Drop(keyID int64)`

- [ ] **Step 1: Write the failing tests**

Write `internal/relay/park_test.go`:

```go
package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func testRequest(id string) ParkedRequest {
	return ParkedRequest{ID: id, Method: "GET", Path: "/api/sessions", Headers: map[string][]string{}}
}

func TestParkTakeComplete(t *testing.T) {
	q := NewParkingQueue(ParkCapacity)
	ctx := context.Background()
	parked := make(chan *ParkedResponse, 1)
	go func() {
		resp, done, _, err := q.Park(ctx, 7, testRequest("r1"))
		if err != nil {
			return
		}
		defer done()
		parked <- resp
	}()
	req, err := q.Take(ctx, 7, "gen-a", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if req.ID != "r1" {
		t.Fatalf("took %+v", req)
	}
	resp := &ParkedResponse{Status: http.StatusOK}
	streamDone, err := q.Complete(7, "gen-a", "r1", resp)
	if err != nil {
		t.Fatal(err)
	}
	if got := <-parked; got != resp {
		t.Fatal("client did not receive the respond")
	}
	select {
	case <-streamDone:
		t.Fatal("streamDone closed before the client finished")
	default:
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-streamDone:
			return
		case <-deadline:
			t.Fatal("streamDone never closed after done()")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestParkQueueFull(t *testing.T) {
	q := NewParkingQueue(2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 2; i++ {
		go func(i int) {
			_, _, _, _ = q.Park(ctx, 7, testRequest(fmt.Sprintf("r%d", i)))
		}(i)
	}
	deadline := time.After(time.Second)
	for q.Len(7) < 2 {
		select {
		case <-deadline:
			t.Fatal("queue never filled")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if _, _, _, err := q.Park(ctx, 7, testRequest("r3")); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("third park: %v, want ErrQueueFull (client sees 503)", err)
	}
}

func TestParkClientCancelThenRespondGone(t *testing.T) {
	q := NewParkingQueue(ParkCapacity)
	ctx := context.Background()
	clientCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, _, _, err := q.Park(clientCtx, 7, testRequest("r1"))
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if _, err := q.Take(ctx, 7, "gen-a", time.Second); err != nil {
		t.Fatal(err)
	}
	// The client hangs up AFTER the bridge took the request: the bridge's
	// respond must get ErrClientGone semantics (410), never a goroutine
	// leak (design §6.2).
	cancel()
	if err := <-done; err == nil {
		t.Fatal("park returned nil on client cancel")
	}
	if _, err := q.Complete(7, "gen-a", "r1", &ParkedResponse{}); !errors.Is(err, ErrClientGone) {
		t.Fatalf("respond after client cancel: %v, want ErrClientGone", err)
	}
	if n := q.Len(7); n != 0 {
		t.Fatalf("voided item left behind (n=%d)", n)
	}
}

func TestTakeTimeout(t *testing.T) {
	q := NewParkingQueue(ParkCapacity)
	start := time.Now()
	_, err := q.Take(context.Background(), 7, "gen-a", 50*time.Millisecond)
	if !errors.Is(err, ErrTakeTimeout) {
		t.Fatalf("empty take: %v, want ErrTakeTimeout (204)", err)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("take returned before the wait window")
	}
}

func TestFailAll(t *testing.T) {
	q := NewParkingQueue(ParkCapacity)
	ctx := context.Background()
	errs := make(chan error, 2)
	for _, id := range []string{"r1", "r2"} {
		go func(id string) {
			_, _, _, err := q.Park(ctx, 7, testRequest(id))
			errs <- err
		}(id)
	}
	deadline := time.After(time.Second)
	for q.Len(7) < 2 {
		select {
		case <-deadline:
			t.Fatal("queue never filled")
		case <-time.After(5 * time.Millisecond):
		}
	}
	q.FailAll(7, ErrRevoked)
	for i := 0; i < 2; i++ {
		if err := <-errs; !errors.Is(err, ErrRevoked) {
			t.Fatalf("drained park: %v, want ErrRevoked", err)
		}
	}
	if n := q.Len(7); n != 0 {
		t.Fatalf("queue len %d after FailAll", n)
	}
}
```

Write `internal/relay/generation_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
go test ./internal/relay/ -run 'TestPark|TestTake|TestFailAll|TestGeneration|TestRegisterPoll' -count=1
```

Expected failure: compile errors — `undefined: NewParkingQueue`, `undefined: ParkCapacity`, `undefined: ErrQueueFull`, `undefined: NewGenerationRegistry`, `undefined: ErrGenerationConflict`.

- [ ] **Step 3: Write minimal implementation**

Write `internal/relay/park.go`:

```go
package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// ParkCapacity is the per-key parked-request ceiling (design §6.2: 64;
// a full queue answers 503).
const ParkCapacity = 64

var (
	// ErrQueueFull rejects a park beyond the per-key capacity.
	ErrQueueFull = errors.New("relay: parking queue full")
	// ErrTakeTimeout ends a long poll with no parked request (204).
	ErrTakeTimeout = errors.New("relay: park take timed out")
	// ErrUnknownParkedID rejects a respond for an id this key+generation
	// does not hold (404 — cross-key / cross-generation respond forgery
	// is impossible, design §6.1).
	ErrUnknownParkedID = errors.New("relay: unknown parked request")
	// ErrClientGone marks a respond for a request whose client already
	// disconnected (410 — the bridge unsticks its handler goroutine).
	ErrClientGone = errors.New("relay: client disconnected")
	// ErrRevoked drains the queue on key revocation (design §5.3) and
	// wakes in-flight polls (design §5.1).
	ErrRevoked = errors.New("relay: key revoked")
)

// ParkedRequest is one client request waiting for its bridge.
type ParkedRequest struct {
	ID       string
	Method   string
	Path     string
	Query    string
	ClientIP string
	Headers  map[string][]string
	Body     []byte
}

// ParkedResponse is the bridge's streamed answer handed to the parked
// client: status + headers freeze first, the body streams chunk by chunk.
type ParkedResponse struct {
	Status  int
	Headers http.Header
	Body    io.ReadCloser
}

type parkResult struct {
	resp *ParkedResponse
	err  error
}

type parkedItem struct {
	req        ParkedRequest
	result     chan parkResult
	failCause  chan error // buffered 1: revocation cause for the stream consumer
	streamDone chan struct{}
	inFlight   bool
	generation string
	voided     bool
	resp       *ParkedResponse
}

// ParkingQueue holds per-key parked client requests (design §6.2): a
// client Park blocks until the bridge's respond completes it; a bridge
// Take long-polls for work.
type ParkingQueue struct {
	mu       sync.Mutex
	capacity int
	byKey    map[int64][]*parkedItem
	changed  chan struct{}
}

// NewParkingQueue builds a queue with the given per-key capacity.
func NewParkingQueue(capacity int) *ParkingQueue {
	return &ParkingQueue{capacity: capacity, byKey: map[int64][]*parkedItem{}, changed: make(chan struct{})}
}

// broadcastLocked wakes every Take waiter.
func (q *ParkingQueue) broadcastLocked() {
	close(q.changed)
	q.changed = make(chan struct{})
}

// Park enqueues req and blocks until the bridge answers, the request
// context ends (client disconnect), or the queue is drained (revocation).
// The returned done func MUST be called when the response stream has been
// fully consumed — it releases the bridge's respond handler. failCause
// delivers the revocation cause to the stream consumer (the SSE error
// injection keys off it — deterministic, independent of read-error
// flavor).
func (q *ParkingQueue) Park(ctx context.Context, keyID int64, req ParkedRequest) (*ParkedResponse, func(), <-chan error, error) {
	item := &parkedItem{
		req:        req,
		result:     make(chan parkResult, 1),
		failCause:  make(chan error, 1),
		streamDone: make(chan struct{}),
	}
	q.mu.Lock()
	if len(q.byKey[keyID]) >= q.capacity {
		q.mu.Unlock()
		return nil, nil, nil, ErrQueueFull
	}
	q.byKey[keyID] = append(q.byKey[keyID], item)
	q.broadcastLocked()
	q.mu.Unlock()

	closeStream := func() {
		select {
		case <-item.streamDone:
		default:
			close(item.streamDone)
		}
	}
	done := func() {
		q.mu.Lock()
		q.removeLocked(keyID, item)
		q.mu.Unlock()
		closeStream()
	}

	select {
	case res := <-item.result:
		if res.err != nil {
			done()
			return nil, nil, nil, res.err
		}
		return res.resp, done, item.failCause, nil
	case <-ctx.Done():
		q.mu.Lock()
		if item.inFlight {
			// The bridge already took it: keep the shell so its respond
			// gets ErrClientGone (410), not ErrUnknownParkedID.
			item.voided = true
			q.mu.Unlock()
		} else {
			q.removeLocked(keyID, item)
			q.mu.Unlock()
		}
		closeStream()
		return nil, nil, nil, ctx.Err()
	}
}

// Take long-polls for the next parked request of keyID, marking it
// in-flight for generation. ErrTakeTimeout ends the poll (204); a
// cancelled ctx (client hangup or revocation wakeup) returns ctx.Err().
func (q *ParkingQueue) Take(ctx context.Context, keyID int64, generation string, wait time.Duration) (*ParkedRequest, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		q.mu.Lock()
		for _, item := range q.byKey[keyID] {
			if !item.inFlight {
				item.inFlight = true
				item.generation = generation
				q.mu.Unlock()
				return &item.req, nil
			}
		}
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, ErrTakeTimeout
		case <-changed:
		}
	}
}

// Complete hands the bridge's respond to the parked client. The id must
// belong to this key and have been taken by this generation — anything
// else is ErrUnknownParkedID (404); a client that disconnected meanwhile
// yields ErrClientGone (410). The returned channel closes when the client
// side finishes streaming the body.
func (q *ParkingQueue) Complete(keyID int64, generation, id string, resp *ParkedResponse) (<-chan struct{}, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, item := range q.byKey[keyID] {
		if item.req.ID != id {
			continue
		}
		if !item.inFlight || item.generation != generation {
			return nil, ErrUnknownParkedID
		}
		if item.voided {
			q.removeLocked(keyID, item)
			return nil, ErrClientGone
		}
		item.resp = resp
		item.result <- parkResult{resp: resp}
		return item.streamDone, nil
	}
	return nil, ErrUnknownParkedID
}

// FailAll drains every parked request of keyID with err (revocation,
// design §5.3). In-flight items additionally have their streamDone closed
// so the respond handler stops waiting, and their failCause carries err
// to the stream consumer.
func (q *ParkingQueue) FailAll(keyID int64, err error) {
	q.mu.Lock()
	items := q.byKey[keyID]
	delete(q.byKey, keyID)
	q.broadcastLocked()
	q.mu.Unlock()
	for _, item := range items {
		select {
		case item.failCause <- err:
		default:
		}
		select {
		case item.result <- parkResult{err: err}:
		default:
		}
		if item.resp != nil && item.resp.Body != nil {
			_ = item.resp.Body.Close()
		}
		select {
		case <-item.streamDone:
		default:
			close(item.streamDone)
		}
	}
}

// Len reports the parked count for keyID (status + tests).
func (q *ParkingQueue) Len(keyID int64) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.byKey[keyID])
}

func (q *ParkingQueue) removeLocked(keyID int64, item *parkedItem) {
	items := q.byKey[keyID]
	for i, it := range items {
		if it == item {
			q.byKey[keyID] = append(items[:i], items[i+1:]...)
			return
		}
	}
}
```

Write `internal/relay/generation.go`:

```go
package relay

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrGenerationConflict rejects a poll from a second live generation of
// the same key (409 — the new generation backs off and the old one's
// lease expires within GenerationStaleAfter, design §6.2).
var ErrGenerationConflict = errors.New("relay: another bridge generation is live")

type generationState struct {
	token    string
	lastPoll time.Time
	polls    map[int]context.CancelCauseFunc
	nextPoll int
}

// GenerationRegistry tracks each key's live bridge generation and its
// in-flight polls (design §6.2): first-come-first-served with lease
// expiry, plus instant cancellation of parked polls on revocation.
type GenerationRegistry struct {
	mu   sync.Mutex
	now  func() time.Time
	gens map[int64]*generationState
}

// NewGenerationRegistry builds an empty registry (now is swappable in
// tests).
func NewGenerationRegistry() *GenerationRegistry {
	return &GenerationRegistry{now: time.Now, gens: map[int64]*generationState{}}
}

// Claim registers gen as keyID's generation. A different generation that
// is still live (last poll within the lease) conflicts; a stale one is
// evicted and gen takes over. adopted reports gen became the key's
// generation (bridge-up audit hooks off it); evicted reports a stale
// predecessor was displaced (bridge-down audit).
func (r *GenerationRegistry) Claim(keyID int64, gen string) (adopted, evicted bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	st, ok := r.gens[keyID]
	if ok && st.token != gen {
		if now.Sub(st.lastPoll) < GenerationStaleAfter {
			return false, false, ErrGenerationConflict
		}
		evicted = true
	}
	if !ok || st.token != gen {
		st = &generationState{token: gen, lastPoll: now, polls: map[int]context.CancelCauseFunc{}}
		r.gens[keyID] = st
		return true, evicted, nil
	}
	st.lastPoll = now
	return false, false, nil
}

// Current reports whether gen is keyID's live generation (respond
// scoping: a superseded generation's responds are 404, design §6.2).
func (r *GenerationRegistry) Current(keyID int64, gen string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.gens[keyID]
	return ok && st.token == gen
}

// Online is the bridge-online check for client forwarding (design §6.2:
// last poll within the lease window).
func (r *GenerationRegistry) Online(keyID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.gens[keyID]
	return ok && r.now().Sub(st.lastPoll) < GenerationStaleAfter
}

// RegisterPoll tracks one in-flight long poll so revocation can wake it
// instantly (design §5.1). ok is false when gen is no longer current.
// The returned func unregisters.
func (r *GenerationRegistry) RegisterPoll(keyID int64, gen string, cancel context.CancelCauseFunc) (func(), bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.gens[keyID]
	if !ok || st.token != gen {
		return nil, false
	}
	id := st.nextPoll
	st.nextPoll++
	st.polls[id] = cancel
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(st.polls, id)
	}, true
}

// CancelPolls wakes every in-flight poll of keyID with cause (410 revoked
// immediately, without waiting out the 25s poll window, design §5.1).
func (r *GenerationRegistry) CancelPolls(keyID int64, cause error) {
	r.mu.Lock()
	st, ok := r.gens[keyID]
	var cancels []context.CancelCauseFunc
	if ok {
		cancels = make([]context.CancelCauseFunc, 0, len(st.polls))
		for _, c := range st.polls {
			cancels = append(cancels, c)
		}
	}
	r.mu.Unlock()
	for _, c := range cancels {
		c(cause)
	}
}

// Drop removes keyID's generation state (revocation).
func (r *GenerationRegistry) Drop(keyID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.gens, keyID)
}
```

- [ ] **Step 4: Run tests to verify they pass**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
go test ./internal/relay/ -count=1
```

Expected: `ok  	github.com/projanvil/woca/internal/relay` — Tasks 1-4 tests all PASS.

- [ ] **Step 5: Commit**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
git add internal/relay/park.go internal/relay/park_test.go internal/relay/generation.go internal/relay/generation_test.go
git commit -m "relay: parking queue and generation lease registry"
```

---

### Task 5: Tunnel endpoints + client request forwarding

**Files:**
- Create: internal/relay/tunnel.go
- Create: internal/relay/forward.go
- Modify: internal/relay/serve.go (attach the tunnel plane in `NewRelay`; register tunnel routes in `Handler`; extend `route` with session-gated forwarding)
- Test: internal/relay/tunnel_test.go

**Interfaces:**
- Consumes: `KeyStore`/`LookupKey`/`TouchKey` (T1), `AuthService.SessionFromRequest`/`ClientIP`, `writeAuthError`, `WriteError`, `writeJSON`, `Code*` (T2), `Relay` + `route` (T3), `ParkingQueue`/`GenerationRegistry` + all their methods and sentinels (T4).
- Produces (relied on by Tasks 6/7):
  - Wire constants: `GenerationHeader = "X-Woca-Relay-Generation"`, `StatusHeader = "X-Woca-Relay-Status"`, `HeadersHeader = "X-Woca-Relay-Headers"`, `RemoteHeader = "X-Woca-Relay-Remote"`, `DefaultPollWait = 25 * time.Second`, `MaxClientBody = 4 << 20`
  - `type TunnelService struct` + `func NewTunnelService(keys, park, gens, log) *TunnelService`, field `OnBridgeChange func(keyID int64, up bool)`
  - `func (t *TunnelService) HandlePoll(w, r)`, `func (t *TunnelService) HandleRespond(w, r)`
  - `func StripHopByHop(h http.Header)`, `func decodeHeaders(raw string) (http.Header, error)`
  - `type pollPayload struct { ID, Method, Path, Query, Client string; Headers map[string][]string; Body string }` (test-shared with Task 6)
  - `func (rl *Relay) forward(w, r, sess Session)`
  - Test helpers (same package): `newTestRelay(t) *testRelay`, `(*testRelay).login(t) *http.Client`, `(*testRelay).pollOnce(t, gen, wait) (*http.Response, pollPayload)`, `(*testRelay).respond(t, gen, id, status, headers, body) *http.Response`, `sessionTokenOf(t, tr) string`

- [ ] **Step 1: Write the failing tests**

Write `internal/relay/tunnel_test.go`:

```go
package relay

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testRelay is the in-process full assembly (the edgegw_test.go pattern):
// a real Relay over httptest plus a created key.
type testRelay struct {
	ks     *KeyStore
	rl     *Relay
	srv    *httptest.Server
	key    Key
	rawKey string
}

func newTestRelay(t *testing.T) *testRelay {
	t.Helper()
	ks := openTestKeyStore(t)
	rl := NewRelay(ks, testSPA(), false, testLogger())
	srv := httptest.NewServer(rl.Handler())
	t.Cleanup(srv.Close)
	k, raw, err := ks.CreateKey(t.Context(), "home-mac")
	if err != nil {
		t.Fatal(err)
	}
	return &testRelay{ks: ks, rl: rl, srv: srv, key: k, rawKey: raw}
}

// login returns a client whose cookie jar carries the session.
func (tr *testRelay) login(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: jar}
	resp, err := c.Post(tr.srv.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"api_key":"`+tr.rawKey+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	return c
}

// sessionTokenOf logs in and extracts the cookie value for handcrafted
// requests.
func sessionTokenOf(t *testing.T, tr *testRelay) string {
	t.Helper()
	resp, err := http.Post(tr.srv.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"api_key":"`+tr.rawKey+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookieName {
			return c.Value
		}
	}
	t.Fatal("login issued no cookie")
	return ""
}

// claimGen parks a quick poll so the generation registers (the forward
// path's online check needs it).
func (tr *testRelay) claimGen(t *testing.T, gen string) {
	t.Helper()
	resp, _ := tr.pollOnce(t, gen, 10*time.Millisecond)
	_ = resp.Body.Close()
}

// pollOnce performs one bridge poll.
func (tr *testRelay) pollOnce(t *testing.T, gen string, wait time.Duration) (*http.Response, pollPayload) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/tunnel/poll?wait=%s", tr.srv.URL, wait), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tr.rawKey)
	req.Header.Set(GenerationHeader, gen)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var p pollPayload
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	return resp, p
}

// respond uploads one bridge answer; body may stream.
func (tr *testRelay) respond(t *testing.T, gen, id string, status int, headers http.Header, body io.Reader) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(map[string][]string(headers))
	req, err := http.NewRequest(http.MethodPost, tr.srv.URL+"/tunnel/respond/"+id, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tr.rawKey)
	req.Header.Set(GenerationHeader, gen)
	req.Header.Set(StatusHeader, fmt.Sprint(status))
	req.Header.Set(HeadersHeader, base64.StdEncoding.EncodeToString(raw))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestTunnelRoundTrip(t *testing.T) {
	tr := newTestRelay(t)
	client := tr.login(t)
	tr.claimGen(t, "gen-1")

	go func() {
		resp, p := tr.pollOnce(t, "gen-1", 5*time.Second)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return
		}
		r := tr.respond(t, "gen-1", p.ID, http.StatusOK,
			http.Header{"Content-Type": {"application/json"}},
			strings.NewReader(`{"ok":true}`))
		_ = r.Body.Close()
	}()
	time.Sleep(100 * time.Millisecond)

	resp, err := client.Get(tr.srv.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tunneled GET: %d", resp.StatusCode)
	}
	data, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(data), `"ok":true`) {
		t.Fatalf("body: %s", data)
	}
}

func TestTunnelSSE(t *testing.T) {
	tr := newTestRelay(t)
	client := tr.login(t)
	tr.claimGen(t, "gen-1")

	go func() {
		resp, p := tr.pollOnce(t, "gen-1", 5*time.Second)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return
		}
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte("data: one\n\n"))
			time.Sleep(300 * time.Millisecond)
			_, _ = pw.Write([]byte("data: two\n\n"))
			_ = pw.Close()
		}()
		// The snapshot carries woca's SSE quirk (sse.go:30): Connection:
		// keep-alive must be filtered hop-by-hop (design §6.3).
		r := tr.respond(t, "gen-1", p.ID, http.StatusOK, http.Header{
			"Content-Type": {"text/event-stream"},
			"Connection":   {"keep-alive"},
		}, pr)
		_ = r.Body.Close()
	}()
	time.Sleep(100 * time.Millisecond)

	resp, err := client.Get(tr.srv.URL + "/api/runs/run-1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q (the flush-before-headers order must not lose it)", ct)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering %q, want no", got)
	}
	if got := resp.Header.Get("Connection"); got != "" {
		t.Fatalf("hop-by-hop Connection leaked to the client: %q", got)
	}
	// Chunk immediacy: frame one arrives well before frame two is sent
	// (no aggregation, no buffering — design §6.3).
	start := time.Now()
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "data: one") {
		t.Fatalf("first chunk: n=%d err=%v", n, err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("first frame took %v — the relay buffered instead of flushing", elapsed)
	}
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "data: two") {
		t.Fatalf("stream tail: %q", rest)
	}
}

func TestTunnelAuth(t *testing.T) {
	tr := newTestRelay(t)

	// No bearer at all.
	req, _ := http.NewRequest(http.MethodGet, tr.srv.URL+"/tunnel/poll", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous poll: %d, want 401", resp.StatusCode)
	}
	// Unknown key.
	req.Header.Set("Authorization", "Bearer wcr_"+strings.Repeat("A", 43))
	req.Header.Set(GenerationHeader, "gen-1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown-key poll: %d, want 401", resp.StatusCode)
	}
	// Revoked key: 410 with the frontend-contract code.
	if _, ok, err := tr.ks.RevokeKey(t.Context(), "home-mac"); err != nil || !ok {
		t.Fatal("revoke setup failed")
	}
	req.Header.Set("Authorization", "Bearer "+tr.rawKey)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusGone || !strings.Contains(string(body), `"code":"revoked"`) {
		t.Fatalf("revoked poll: %d %s, want 410 + code revoked", resp.StatusCode, body)
	}
	// Client API without a session: 401 (the login gate).
	resp, err = http.Get(tr.srv.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous API: %d, want 401", resp.StatusCode)
	}
	// The anonymous whitelist still answers.
	resp, err = http.Get(tr.srv.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health: %d, want 200", resp.StatusCode)
	}
}

func TestGenerationConflictAndTakeover(t *testing.T) {
	tr := newTestRelay(t)
	now := time.Now()
	tr.rl.gens.now = func() time.Time { return now } // test clock

	tr.claimGen(t, "gen-a")
	// Competing live generation: 409 (design §6.2 / §9 matrix).
	resp, _ := tr.pollOnce(t, "gen-b", 10*time.Millisecond)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("competing generation: %d, want 409", resp.StatusCode)
	}
	// The lease expires; gen-b takes over.
	now = now.Add(GenerationStaleAfter + time.Second)
	tr.claimGen(t, "gen-b")
	// gen-a is a zombie: its respond 404s.
	r := tr.respond(t, "gen-a", "whatever", 200, nil, nil)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("zombie respond: %d, want 404", r.StatusCode)
	}
}

func TestRespondIsBridgeScoped(t *testing.T) {
	tr := newTestRelay(t)
	_, otherRaw, err := tr.ks.CreateKey(t.Context(), "other")
	if err != nil {
		t.Fatal(err)
	}
	client := tr.login(t)
	tr.claimGen(t, "gen-1")

	taken := make(chan pollPayload, 1)
	go func() {
		resp, p := tr.pollOnce(t, "gen-1", 5*time.Second)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			taken <- p
		}
	}()
	time.Sleep(100 * time.Millisecond)
	go func() {
		resp, err := client.Get(tr.srv.URL + "/api/workspace")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	var p pollPayload
	select {
	case p = <-taken:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge never received the parked request")
	}
	// Cross-generation respond: 404 (design §6.1).
	r := tr.respond(t, "gen-2", p.ID, 200, nil, strings.NewReader("{}"))
	_ = r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-generation respond: %d, want 404", r.StatusCode)
	}
	// Cross-key respond: the other key's generation is not current → 404.
	req, _ := http.NewRequest(http.MethodPost, tr.srv.URL+"/tunnel/respond/"+p.ID,
		strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+otherRaw)
	req.Header.Set(GenerationHeader, "gen-9")
	req.Header.Set(StatusHeader, "200")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-key respond: %d, want 404", resp2.StatusCode)
	}
	// Unknown id on the right key+generation: 404.
	r = tr.respond(t, "gen-1", "made-up", 200, nil, strings.NewReader("{}"))
	_ = r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown-id respond: %d, want 404", r.StatusCode)
	}
}

func TestClientBodyLimit(t *testing.T) {
	tr := newTestRelay(t)
	client := tr.login(t)
	tr.claimGen(t, "gen-1")

	big := bytes.NewReader(make([]byte, MaxClientBody+1))
	resp, err := client.Post(tr.srv.URL+"/api/runs", "application/octet-stream", big)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("4MB+1 body: %d, want 413", resp.StatusCode)
	}
	if n := tr.rl.park.Len(tr.key.ID); n != 0 {
		t.Fatalf("oversized request was parked (n=%d)", n)
	}
}

func TestBridgeOffline(t *testing.T) {
	tr := newTestRelay(t)
	client := tr.login(t)
	resp, err := client.Get(tr.srv.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable ||
		!strings.Contains(string(body), `"code":"bridge_offline"`) ||
		resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("offline: %d %s retry=%q (want 503 bridge_offline + Retry-After 5)",
			resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
}

func TestCredentialsStrippedBeforeParking(t *testing.T) {
	tr := newTestRelay(t)
	tr.claimGen(t, "gen-1")

	taken := make(chan pollPayload, 1)
	go func() {
		resp, p := tr.pollOnce(t, "gen-1", 5*time.Second)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			taken <- p
			r := tr.respond(t, "gen-1", p.ID, 200, nil, strings.NewReader("{}"))
			_ = r.Body.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond)

	// Handcrafted request carrying BOTH credential channels (design §6.4:
	// the relay's own session credentials never cross the tunnel).
	req, _ := http.NewRequest(http.MethodGet, tr.srv.URL+"/api/sessions", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionTokenOf(t, tr)})
	req.Header.Set("Authorization", "Bearer native-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forward: %d", resp.StatusCode)
	}
	p := <-taken
	for name := range p.Headers {
		if strings.EqualFold(name, "Cookie") || strings.EqualFold(name, "Authorization") {
			t.Fatalf("credential header %q crossed the tunnel", name)
		}
	}
	if p.Client == "" {
		t.Fatal("client IP not propagated to the bridge")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
go test ./internal/relay/ -run 'TestTunnel|TestGeneration|TestRespond|TestClientBody|TestBridgeOffline|TestCredentials' -count=1
```

Expected failure: compile errors — `undefined: pollPayload`, `undefined: GenerationHeader`, `undefined: StatusHeader`, `undefined: HeadersHeader`, `undefined: MaxClientBody`, plus `tr.rl.park` / `tr.rl.gens` nil-pointer panics if only partially implemented (the `NewRelay` attach edit is part of this task).

- [ ] **Step 3: Write minimal implementation**

Write `internal/relay/tunnel.go`:

```go
package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// Tunnel wire vocabulary (design §6.1). internal/relayclient carries its
// own copies of these constants — the two packages stay independently
// compilable (the same discipline as the enterprise bridge/edge pair).
const (
	GenerationHeader = "X-Woca-Relay-Generation"
	StatusHeader     = "X-Woca-Relay-Status"
	HeadersHeader    = "X-Woca-Relay-Headers"
	RemoteHeader     = "X-Woca-Relay-Remote"
)

// DefaultPollWait bounds one long poll when the bridge does not pass
// ?wait= (design §6.1: 25s).
const DefaultPollWait = 25 * time.Second

// TunnelService owns the bridge-facing endpoints: GET /tunnel/poll and
// POST /tunnel/respond/{id}.
type TunnelService struct {
	keys *KeyStore
	park *ParkingQueue
	gens *GenerationRegistry
	log  zerolog.Logger
	// OnBridgeChange fires on generation adopt (up=true) and stale
	// eviction (up=false) — the revocation task hooks the audit log here.
	OnBridgeChange func(keyID int64, up bool)
}

// NewTunnelService builds the tunnel plane.
func NewTunnelService(keys *KeyStore, park *ParkingQueue, gens *GenerationRegistry, log zerolog.Logger) *TunnelService {
	return &TunnelService{keys: keys, park: park, gens: gens, log: log}
}

// pollPayload is the parked request's wire shape (the 200 body of
// /tunnel/poll).
type pollPayload struct {
	ID      string              `json:"id"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Query   string              `json:"query"`
	Client  string              `json:"client"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"` // base64 standard encoding
}

// authenticate resolves the Bearer key for a tunnel request: 401 for
// unknown keys, 410 {"code":"revoked"} for revoked ones (design §9).
func (t *TunnelService) authenticate(w http.ResponseWriter, r *http.Request) (Key, bool) {
	h := r.Header.Get("Authorization")
	if len(h) <= 7 || !strings.EqualFold(h[:7], "bearer ") {
		WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "missing bearer key")
		return Key{}, false
	}
	key, err := t.keys.LookupKey(r.Context(), strings.TrimSpace(h[7:]))
	if errors.Is(err, ErrKeyRevoked) {
		WriteError(w, http.StatusGone, CodeRevoked, "key revoked")
		return Key{}, false
	}
	if err != nil {
		WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "unknown key")
		return Key{}, false
	}
	return key, true
}

// HandlePoll implements GET /tunnel/poll?wait=25s (design §6.1): Bearer
// key + generation header, generation claim (409 on a competing live
// generation), then a long-poll take — 204 on timeout, 410 instantly on
// revocation wakeup.
func (t *TunnelService) HandlePoll(w http.ResponseWriter, r *http.Request) {
	key, ok := t.authenticate(w, r)
	if !ok {
		return
	}
	gen := r.Header.Get(GenerationHeader)
	if gen == "" {
		WriteError(w, http.StatusBadRequest, CodeUnauthorized, "missing "+GenerationHeader)
		return
	}
	adopted, evicted, err := t.gens.Claim(key.ID, gen)
	if errors.Is(err, ErrGenerationConflict) {
		WriteError(w, http.StatusConflict, CodeUnauthorized, "another bridge generation is live")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeUnauthorized, "generation claim failed")
		return
	}
	if t.OnBridgeChange != nil {
		if evicted {
			t.OnBridgeChange(key.ID, false)
		}
		if adopted {
			t.OnBridgeChange(key.ID, true)
		}
	}
	t.keys.TouchKey(r.Context(), key.ID)

	wait := DefaultPollWait
	if raw := r.URL.Query().Get("wait"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 && d <= time.Minute {
			wait = d
		}
	}
	pollCtx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	unregister, ok := t.gens.RegisterPoll(key.ID, gen, cancel)
	if !ok {
		WriteError(w, http.StatusConflict, CodeUnauthorized, "generation superseded")
		return
	}
	defer unregister()

	req, err := t.park.Take(pollCtx, key.ID, gen, wait)
	if errors.Is(err, ErrTakeTimeout) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		if errors.Is(context.Cause(pollCtx), ErrRevoked) {
			// Revocation wakeup: 410 immediately, not after the wait
			// window (design §5.1).
			WriteError(w, http.StatusGone, CodeRevoked, "key revoked")
			return
		}
		// The bridge hung up mid-poll: nothing to write.
		return
	}
	writeJSON(w, http.StatusOK, pollPayload{
		ID:      req.ID,
		Method:  req.Method,
		Path:    req.Path,
		Query:   req.Query,
		Client:  req.ClientIP,
		Headers: req.Headers,
		Body:    base64.StdEncoding.EncodeToString(req.Body),
	})
}

// HandleRespond implements POST /tunnel/respond/{id} (design §6.1/§6.3):
// the id must belong to this Bearer key's current generation (404
// otherwise), headers arrive base64-JSON in X-Woca-Relay-Headers with
// hop-by-hop filtering, and the body streams to the parked client.
func (t *TunnelService) HandleRespond(w http.ResponseWriter, r *http.Request) {
	key, ok := t.authenticate(w, r)
	if !ok {
		return
	}
	gen := r.Header.Get(GenerationHeader)
	if gen == "" || !t.gens.Current(key.ID, gen) {
		WriteError(w, http.StatusNotFound, CodeUnauthorized, "unknown parked request")
		return
	}
	t.keys.TouchKey(r.Context(), key.ID)

	status, err := strconv.Atoi(r.Header.Get(StatusHeader))
	if err != nil || status < 100 || status > 999 {
		WriteError(w, http.StatusBadRequest, CodeUnauthorized, "invalid "+StatusHeader)
		return
	}
	headers, err := decodeHeaders(r.Header.Get(HeadersHeader))
	if err != nil {
		WriteError(w, http.StatusBadRequest, CodeUnauthorized, "invalid "+HeadersHeader)
		return
	}
	streamDone, err := t.park.Complete(key.ID, gen, r.PathValue("id"), &ParkedResponse{
		Status:  status,
		Headers: headers,
		Body:    r.Body,
	})
	if errors.Is(err, ErrClientGone) {
		WriteError(w, http.StatusGone, CodeUnauthorized, "client disconnected")
		return
	}
	if err != nil {
		WriteError(w, http.StatusNotFound, CodeUnauthorized, "unknown parked request")
		return
	}
	// Stay attached until the client side finishes the body — returning
	// early would truncate the stream. Revocation closes streamDone and
	// the body close unsticks the parked client (design §5).
	select {
	case <-streamDone:
		w.WriteHeader(http.StatusOK)
	case <-r.Context().Done():
	}
}

// decodeHeaders restores the base64-JSON header snapshot and strips
// hop-by-hop headers.
func decodeHeaders(raw string) (http.Header, error) {
	h := http.Header{}
	if raw == "" {
		return h, nil
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	var m map[string][]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	for k, vs := range m {
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	StripHopByHop(h)
	return h, nil
}

// StripHopByHop removes connection-specific headers in place (design
// §6.3: Connection, Keep-Alive, Transfer-Encoding, TE, Trailer, Upgrade —
// plus anything the Connection token names. woca's SSE path sets
// Connection: keep-alive; forwarding it verbatim to the browser is a
// protocol violation).
func StripHopByHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for name := range strings.SplitSeq(v, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Keep-Alive", "Transfer-Encoding", "TE", "Trailer", "Upgrade"} {
		h.Del(name)
	}
}
```

Write `internal/relay/forward.go`:

```go
package relay

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// MaxClientBody is the client→bridge request body cap (design §6.3: 4
// MB, 413 beyond; streaming uploads are YAGNI).
const MaxClientBody = 4 << 20

// bridgeOfflineRetryAfter is the Retry-After hint when no bridge
// generation is online (design §9 error matrix).
const bridgeOfflineRetryAfter = 5

// forward relays one authenticated client request through the parking
// plane to the bridge and streams its answer back (design §6.2/§6.3).
func (rl *Relay) forward(w http.ResponseWriter, r *http.Request, sess Session) {
	if !rl.gens.Online(sess.KeyID) {
		w.Header().Set("Retry-After", strconv.Itoa(bridgeOfflineRetryAfter))
		WriteError(w, http.StatusServiceUnavailable, CodeBridgeOffline, "bridge offline")
		return
	}
	body, err := readCappedBody(w, r)
	if err != nil {
		WriteError(w, http.StatusRequestEntityTooLarge, CodeUnauthorized, "request body too large")
		return
	}
	headers := r.Header.Clone()
	// The relay's own session credentials mean nothing to the bridge and
	// must never cross the tunnel (design §6.4).
	headers.Del("Cookie")
	headers.Del("Authorization")
	resp, done, failed, err := rl.park.Park(r.Context(), sess.KeyID, ParkedRequest{
		ID:       newRequestID(),
		Method:   r.Method,
		Path:     r.URL.Path,
		Query:    r.URL.RawQuery,
		ClientIP: rl.auth.ClientIP(r),
		Headers:  headers,
		Body:     body,
	})
	if errors.Is(err, ErrQueueFull) {
		WriteError(w, http.StatusServiceUnavailable, CodeBridgeOffline, "bridge busy")
		return
	}
	if err != nil {
		// Client disconnect or revocation drain — nothing left to write.
		return
	}
	defer done()

	out := w.Header()
	for k, vs := range resp.Headers {
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	eventStream := strings.HasPrefix(out.Get("Content-Type"), "text/event-stream")
	if eventStream {
		// Keep intermediary proxies from buffering the stream (design §6.3).
		out.Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(resp.Status)
	defer func() { _ = resp.Body.Close() }()
	flusher, _ := w.(http.Flusher)
	// Client disconnect mid-stream: closing the body unblocks Read so the
	// loop exits promptly instead of pinning the bridge's respond.
	stopWatch := context.AfterFunc(r.Context(), func() { _ = resp.Body.Close() })
	defer stopWatch()
	_ = streamChunks(w, flusher, resp.Body)
	// Revocation mid-stream: inject the revocation event before closing
	// (design §5.2). The frame MUST use the SPA's own vocabulary —
	// `event: auth_revoked` with {"code":"revoked"} — which is exactly
	// what web/src/api/streaming.ts:151-159 switches on (constant
	// EventAuthRevoked, internal/server/ssevents.go:23); any other event
	// name and the revoked notice never fires. failCause is
	// deterministic; read-error flavor is not.
	select {
	case cause := <-failed:
		if eventStream && errors.Is(cause, ErrRevoked) {
			_, _ = fmt.Fprint(w, "event: auth_revoked\ndata: {\"code\":\"revoked\"}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	default:
	}
}

// streamChunks copies body to w, flushing after every chunk — no
// aggregation, no buffering (design §6.3).
func streamChunks(w http.ResponseWriter, flusher http.Flusher, body io.Reader) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// readCappedBody enforces MaxClientBody (413 beyond, design §6.3).
func readCappedBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(http.MaxBytesReader(w, r.Body, MaxClientBody))
}

// newRequestID mints a parked-request id (128-bit CSPRNG, base64url).
func newRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
}
```

Modify `internal/relay/serve.go` — replace `NewRelay` and `route`, and register the tunnel routes in `Handler`:

```go
// NewRelay builds the full surface: auth + SPA + health + the tunnel
// plane (parking queue, generation registry, tunnel endpoints).
func NewRelay(keys *KeyStore, spaFS fs.FS, behindProxy bool, log zerolog.Logger) *Relay {
	park := NewParkingQueue(ParkCapacity)
	gens := NewGenerationRegistry()
	return &Relay{
		keys:        keys,
		auth:        NewAuthService(keys, behindProxy, log),
		spa:         webfx.SPA(spaFS),
		behindProxy: behindProxy,
		log:         log,
		park:        park,
		gens:        gens,
		tunnel:      NewTunnelService(keys, park, gens, log),
	}
}
```

```go
func (rl *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "woca-relay"})
	})
	mux.HandleFunc("POST /api/auth/login", rl.auth.HandleLogin)
	mux.HandleFunc("POST /api/auth/logout", rl.auth.HandleLogout)
	mux.HandleFunc("GET /api/auth/me", rl.auth.HandleMe)
	mux.HandleFunc("GET /tunnel/poll", rl.tunnel.HandlePoll)
	mux.HandleFunc("POST /tunnel/respond/{id}", rl.tunnel.HandleRespond)
	mux.HandleFunc("/", rl.route)
	return mux
}

// route: authenticated /api/ traffic forwards through the tunnel; the SPA
// gets everything else (and 404s unknown /api/ paths itself — but with
// the tunnel attached, every /api/ path requires a session first).
func (rl *Relay) route(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") && rl.tunnel != nil {
		sess, err := rl.auth.SessionFromRequest(r)
		if err != nil {
			writeAuthError(w, err)
			return
		}
		rl.forward(w, r, sess)
		return
	}
	rl.spa.ServeHTTP(w, r)
}
```

- [ ] **Step 4: Run tests, apply the skeleton-gate test edit, re-run**

(a) First run — one expected failure:

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
go test ./internal/relay/ -count=1
```

Expected: every test passes EXCEPT `TestRelaySkeletonSurface` (Task 3), which now fails at its `/api/models` assertion — with the tunnel plane attached, that path is session-gated and returns 401 instead of the skeleton's SPA-miss 404.

(b) Discrete edit in `internal/relay/serve_test.go` (inside `TestRelaySkeletonSurface`): replace

```go
	// An /api/ miss is a 404, never the app shell (webfx.SPA semantics —
	// exactly what the relay wants).
	if code, _, _ := get("/api/models"); code != http.StatusNotFound {
		t.Fatalf("anonymous /api/models: %d, want 404 in the skeleton", code)
	}
```

with

```go
	// The tunnel plane gates every /api/ path behind a session (design
	// §7.2 — the whitelist is exactly SPA assets + /api/health +
	// /api/auth/login).
	if code, _, body := get("/api/models"); code != http.StatusUnauthorized ||
		!strings.Contains(body, `"code":"unauthorized"`) {
		t.Fatalf("anonymous /api/models: %d %s, want 401 with the session gate attached", code, body)
	}
```

(c) Re-run:

```sh
go test ./internal/relay/ -count=1
```

Expected: `ok  	github.com/projanvil/woca/internal/relay` — all tests PASS, including `TestTunnelSSE` (chunk immediacy < 200ms, `auth_revoked` injection is Task 6) and the updated `TestRelaySkeletonSurface`.

- [ ] **Step 5: Commit**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
git add internal/relay/tunnel.go internal/relay/forward.go internal/relay/serve.go internal/relay/tunnel_test.go internal/relay/serve_test.go
git commit -m "relay: tunnel endpoints and client request forwarding"
```

---

### Task 6: Revocation cascade + audit log

**Files:**
- Create: internal/relay/audit.go
- Create: internal/relay/revoke.go
- Modify: internal/relay/keystore.go (append `ListRevokedKeys` + `KeyByID`)
- Modify: internal/relay/authhttp.go (add `Audit` field + login events)
- Modify: internal/relay/serve.go (append `AttachRevocation`)
- Modify: cmd/woca-relay/serve.go (wire audit log + revocation watcher)
- Test: internal/relay/revoke_test.go

**Interfaces:**
- Consumes: everything from Tasks 1-5, notably `KeyStore.RevokeKey` (T1), `ResolveSession`'s revoked-via-join behavior (T2 — the tombstone mechanism; `DeleteSessionsForKey` stays as API for maintenance but is deliberately NOT used by the cascade), `ParkingQueue.FailAll`, `GenerationRegistry.CancelPolls/Drop`, `ErrRevoked` (T4), `testRelay` helpers (T5), `TunnelService.OnBridgeChange` (T5).
- Produces:
  - `const AuditLoginFailed = "login_failed"`, `AuditLoginOK = "login_ok"`, `AuditRevocation = "revocation"`, `AuditBridgeUp = "bridge_up"`, `AuditBridgeDown = "bridge_down"`
  - `func NewAuditLog(path string) (*AuditLog, error)`, `func (a *AuditLog) Event(kind, keyName, detail string)`, `func (a *AuditLog) Close() error`
  - `func NewRevoker(keys, gens, park, audit) *Revoker`, `func (rv *Revoker) Revoke(ctx, k Key)`, `func (rv *Revoker) Sweep(ctx) error`, `func (rv *Revoker) Watch(ctx, interval)`
  - `func (s *KeyStore) ListRevokedKeys(ctx) ([]Key, error)`, `func (s *KeyStore) KeyByID(ctx, id int64) (Key, error)`
  - `func (rl *Relay) AttachRevocation(audit *AuditLog) *Revoker`
  - `AuthService.Audit *AuditLog` field

- [ ] **Step 1: Write the failing tests**

Write `internal/relay/revoke_test.go`:

```go
package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRevocationCascade(t *testing.T) {
	tr := newTestRelay(t)
	client := tr.login(t)
	auditLog, err := NewAuditLog("")
	if err != nil {
		t.Fatal(err)
	}
	revoker := NewRevoker(tr.ks, tr.rl.gens, tr.rl.park, auditLog)

	// Bridge: one long poll parked mid-wait when the revoke lands.
	pollResult := make(chan int, 1)
	go func() {
		resp, _ := tr.pollOnce(t, "gen-1", 30*time.Second)
		pollResult <- resp.StatusCode
		_ = resp.Body.Close()
	}()
	time.Sleep(100 * time.Millisecond)

	// Client: one forwarded SSE held open by the bridge across the revoke.
	hold := make(chan struct{})
	sseDone := make(chan string, 1)
	go func() {
		resp, p := tr.pollOnce(t, "gen-1", 5*time.Second)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return
		}
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte("data: hello\n\n"))
			<-hold // the stream stays open across the revoke
			_ = pw.Close()
		}()
		r := tr.respond(t, "gen-1", p.ID, http.StatusOK,
			http.Header{"Content-Type": {"text/event-stream"}}, pr)
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		sseDone <- fmt.Sprintf("%d %s", r.StatusCode, body)
	}()
	time.Sleep(100 * time.Millisecond)

	sseResp, err := client.Get(tr.srv.URL + "/api/runs/run-1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sseResp.Body.Close() }()
	first := make([]byte, 64)
	n, _ := sseResp.Body.Read(first)
	if !strings.Contains(string(first[:n]), "data: hello") {
		t.Fatalf("pre-revoke frame: %q", first[:n])
	}

	// Revoke: the CLI writes revoked_at; the sweeper cascades (design §5).
	if _, ok, err := tr.ks.RevokeKey(t.Context(), "home-mac"); err != nil || !ok {
		t.Fatal("revoke failed")
	}
	if err := revoker.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(hold)

	// 1. The in-flight poll answered 410 immediately — not after 30s.
	select {
	case code := <-pollResult:
		if code != http.StatusGone {
			t.Fatalf("in-flight poll after revoke: %d, want 410", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight poll not woken by revocation")
	}
	// 2. The live SSE got the injected revocation event, then closed:
	// the frame must be `event: auth_revoked` with {"code":"revoked"} —
	// exactly what web/src/api/streaming.ts:151-159 switches on.
	tail, err := io.ReadAll(sseResp.Body)
	if err != nil {
		t.Fatalf("SSE tail read: %v", err)
	}
	if !strings.Contains(string(tail), "event: auth_revoked\ndata: {\"code\":\"revoked\"}") {
		t.Fatalf("SSE tail: %q (auth_revoked frame must be injected)", tail)
	}
	// 3. The bridge's respond handler unblocked (no goroutine leak).
	select {
	case <-sseDone:
	case <-time.After(3 * time.Second):
		t.Fatal("respond handler still blocked after revoke")
	}
	// 4. The revoked session now resolves to ErrKeyRevoked through the
	// keys join (tombstoning — the session row stays so the code is
	// determinable): 401 with the frontend-contract code, cookie and
	// Bearer alike.
	resp, err := client.Get(tr.srv.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(string(body), `"code":"revoked"`) {
		t.Fatalf("post-revoke API: %d %s, want 401 + code revoked", resp.StatusCode, body)
	}
	// 5. New polls get 410 with the frontend contract code.
	resp, _ = tr.pollOnce(t, "gen-2", 10*time.Millisecond)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusGone || !strings.Contains(string(body), `"code":"revoked"`) {
		t.Fatalf("post-revoke poll: %d %s, want 410 + code revoked", resp.StatusCode, body)
	}
	// 6. The cascade is idempotent (a second sweep does nothing new).
	if err := revoker.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAuditLogAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	a, err := NewAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	a.Event(AuditLoginFailed, "", "1.2.3.4")
	a.Event(AuditRevocation, "home-mac", "key revoked")
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("audit lines: %d, want 2", len(lines))
	}
	for i, line := range lines {
		var entry struct {
			Ts     string `json:"ts"`
			Event  string `json:"event"`
			Key    string `json:"key"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("line %d not JSON: %v", i, err)
		}
		if entry.Ts == "" || entry.Event == "" {
			t.Fatalf("line %d incomplete: %+v", i, entry)
		}
	}
	// Append-only: a second open adds, never truncates (design §5.4).
	a2, err := NewAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	a2.Event(AuditBridgeUp, "home-mac", "")
	if err := a2.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if got := len(strings.Split(strings.TrimSpace(string(data)), "\n")); got != 3 {
		t.Fatalf("audit not append-only: %d lines, want 3", got)
	}
}

func TestLoginAuditEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	auditLog, err := NewAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	ks := openTestKeyStore(t)
	_, raw, err := ks.CreateKey(t.Context(), "home-mac")
	if err != nil {
		t.Fatal(err)
	}
	a := NewAuthService(ks, false, testLogger())
	a.Audit = auditLog

	loginFor(t, a, "wcr_"+strings.Repeat("A", 43)) // failure
	loginFor(t, a, raw)                           // success
	_ = auditLog.Close()

	data, _ := os.ReadFile(path)
	out := string(data)
	if !strings.Contains(out, `"event":"login_failed"`) {
		t.Fatalf("login failure not audited: %s", out)
	}
	if !strings.Contains(out, `"event":"login_ok"`) {
		t.Fatalf("login success not audited: %s", out)
	}
	if strings.Contains(out, raw) {
		t.Fatal("audit log leaked the full key (metadata only, design §4.1)")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
go test ./internal/relay/ -run 'TestRevocation|TestAuditLog|TestLoginAudit' -count=1
```

Expected failure: compile errors — `undefined: NewAuditLog`, `undefined: NewRevoker`, `undefined: AuditLoginFailed`, `a.Audit undefined (type *AuthService has no field or method Audit)`.

- [ ] **Step 3: Write minimal implementation**

Write `internal/relay/audit.go`:

```go
package relay

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Audit event kinds (design §5.4: login failures, revocations, bridge
// up/down).
const (
	AuditLoginFailed = "login_failed"
	AuditLoginOK     = "login_ok"
	AuditRevocation  = "revocation"
	AuditBridgeUp    = "bridge_up"
	AuditBridgeDown  = "bridge_down"
)

// AuditLog is the relay's append-only local audit trail (design §5.4):
// one JSON object per line. It records metadata only — never key
// material, never session tokens.
type AuditLog struct {
	mu sync.Mutex
	w  io.Writer
	c  io.Closer
}

// NewAuditLog opens path for appending (0600). An empty path discards
// entries — tests and ephemeral deployments stay quiet.
func NewAuditLog(path string) (*AuditLog, error) {
	if path == "" {
		return &AuditLog{w: io.Discard}, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("relay: audit log: %w", err)
	}
	return &AuditLog{w: f, c: f}, nil
}

// Event appends one audit line: {"ts","event","key","detail"}.
func (a *AuditLog) Event(kind, keyName, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = json.NewEncoder(a.w).Encode(map[string]any{
		"ts":     time.Now().UTC().Format(time.RFC3339),
		"event":  kind,
		"key":    keyName,
		"detail": detail,
	})
}

// Close flushes and closes the underlying file (nil-safe for the
// discard writer).
func (a *AuditLog) Close() error {
	if a.c != nil {
		return a.c.Close()
	}
	return nil
}
```

Write `internal/relay/revoke.go`:

```go
package relay

import (
	"context"
	"sync"
	"time"
)

// Revoker performs the revocation cascade (design §5): wake in-flight
// polls (instant 410), drop the generation lease, TOMBSTONE the key's
// sessions (see Revoke below — rows stay so post-revoke requests resolve
// to code "revoked", the SPA's revoked-notice contract), drain parked
// requests with an error, and audit the event. It also sweeps the key
// table so a revoke issued by a separate `woca-relay key revoke` process
// takes effect in the running relay within the sweep interval.
type Revoker struct {
	keys  *KeyStore
	gens  *GenerationRegistry
	park  *ParkingQueue
	audit *AuditLog

	mu   sync.Mutex
	done map[int64]bool
}

// NewRevoker builds the cascade coordinator.
func NewRevoker(keys *KeyStore, gens *GenerationRegistry, park *ParkingQueue, audit *AuditLog) *Revoker {
	return &Revoker{keys: keys, gens: gens, park: park, audit: audit, done: map[int64]bool{}}
}

// Revoke runs the cascade for one already-revoked key row. Idempotent.
func (rv *Revoker) Revoke(ctx context.Context, k Key) {
	rv.mu.Lock()
	if rv.done[k.ID] {
		rv.mu.Unlock()
		return
	}
	rv.done[k.ID] = true
	rv.mu.Unlock()

	// Bridge side first: in-flight polls return 410 immediately (design
	// §5.1), then the lease drops so no generation resurrects.
	rv.gens.CancelPolls(k.ID, ErrRevoked)
	rv.gens.Drop(k.ID)
	// Client side: sessions are TOMBSTONED, not deleted (design §5.2's
	// "delete" is about denying access, not about erasing rows):
	// ResolveSession's keys join already returns ErrKeyRevoked before any
	// expiry check, so every subsequent request with a revoked session —
	// cookie or Bearer — determinably resolves to 401 {"code":"revoked"}
	// and the SPA's revoked notice fires (WocaApp.vue:137). Deleting the
	// rows would make those requests indistinguishable from logged-out
	// (401 "unauthorized") and the notice would never fire.
	//
	// Tombstoned rows are inert and bounded: they can never authenticate,
	// and ResolveSession already deletes any session row it finds expired
	// (the 7d sliding / 30d absolute check runs for live keys). A lazy
	// purge of expired tombstones rides the same path — a revoked key's
	// session rows disappear on their last expiry touch or can be reaped
	// by a future `DELETE FROM sessions WHERE touched_at < ?` maintenance
	// sweep; neither is required for correctness.
	// Parked but unpicked requests release with an error (design §5.3);
	// in-flight streams get the fail cause (the auth_revoked injection).
	rv.park.FailAll(k.ID, ErrRevoked)
	rv.audit.Event(AuditRevocation, k.Name, "key revoked")
}

// Sweep cascades every revoked key not yet handled. The CLI writes
// revoked_at; the running relay observes it here.
func (rv *Revoker) Sweep(ctx context.Context) error {
	revoked, err := rv.keys.ListRevokedKeys(ctx)
	if err != nil {
		return err
	}
	for _, k := range revoked {
		rv.Revoke(ctx, k)
	}
	return nil
}

// Watch sweeps on interval until ctx ends (serve wires 1s; tests drive
// Sweep directly for determinism).
func (rv *Revoker) Watch(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			_ = rv.Sweep(ctx)
		}
	}
}
```

Modify `internal/relay/keystore.go` — append:

```go
// ListRevokedKeys returns every revoked key (the revocation sweeper).
func (s *KeyStore) ListRevokedKeys(ctx context.Context) ([]Key, error) {
	rows, err := s.db.QueryContext(ctx, keySelect+` WHERE revoked_at IS NOT NULL ORDER BY revoked_at`)
	if err != nil {
		return nil, fmt.Errorf("relay: list revoked keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// KeyByID loads one key row by id (audit name resolution).
func (s *KeyStore) KeyByID(ctx context.Context, id int64) (Key, error) {
	k, err := scanKey(s.db.QueryRowContext(ctx, keySelect+` WHERE id = ?`, id))
	if err != nil {
		return Key{}, fmt.Errorf("relay: key %d: %w", id, err)
	}
	return k, nil
}
```

Modify `internal/relay/authhttp.go` — add the `Audit` field to `AuthService` and emit login events. In the struct:

```go
type AuthService struct {
	keys        *KeyStore
	limiter     *RateLimiter
	behindProxy bool
	log         zerolog.Logger
	// Audit receives login success/failure events (design §5.4); nil is
	// quiet (tests, skeleton).
	Audit *AuditLog
}
```

In `HandleLogin`, inside the unknown-key failure branch:

```go
	if err != nil {
		a.log.Warn().Str("ip", ip).Msg("relay: login failed")
		if a.Audit != nil {
			a.Audit.Event(AuditLoginFailed, "", ip)
		}
		WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "invalid key")
		return
	}
```

and after `CreateSession` succeeds (before `http.SetCookie`):

```go
	if a.Audit != nil {
		a.Audit.Event(AuditLoginOK, key.Name, ip)
	}
```

Modify `internal/relay/serve.go` — append:

```go
// AttachRevocation wires the revocation cascade + audit trail (design
// §5): it returns the Revoker the serve command sweeps on an interval so
// CLI-issued revokes take effect in the running relay, and hooks bridge
// up/down plus login events into the audit log.
func (rl *Relay) AttachRevocation(audit *AuditLog) *Revoker {
	rl.audit = audit
	rl.auth.Audit = audit
	rl.tunnel.OnBridgeChange = func(keyID int64, up bool) {
		name := ""
		if k, err := rl.keys.KeyByID(context.Background(), keyID); err == nil {
			name = k.Name
		}
		kind := AuditBridgeUp
		if !up {
			kind = AuditBridgeDown
		}
		audit.Event(kind, name, "")
	}
	return NewRevoker(rl.keys, rl.gens, rl.park, audit)
}
```

(`serve.go` gains a `"context"` import.)

Modify `cmd/woca-relay/serve.go` — inside the serve `RunE`, after `rl := relay.NewRelay(...)` and before `ln, err := net.Listen(...)`:

```go
			// Revocation plane (design §5): the audit trail plus the sweep
			// that turns CLI-issued revokes into the live cascade.
			auditLog, err := relay.NewAuditLog(*dbPath + ".audit.log")
			if err != nil {
				return err
			}
			defer func() { _ = auditLog.Close() }()
			revoker := rl.AttachRevocation(auditLog)
```

and after the `ctx, stop := signal.NotifyContext(...)` / `defer stop()` lines, before the `fmt.Fprintf(os.Stderr, "woca-relay: ...")` line:

```go
			go revoker.Watch(ctx, time.Second)
```

- [ ] **Step 4: Run tests to verify they pass**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
go test ./internal/relay/ -count=1
go build ./cmd/woca-relay
go vet ./internal/relay/ ./cmd/woca-relay
```

Expected: `ok  	github.com/projanvil/woca/internal/relay` (all Tasks 1-6 tests PASS), clean build, clean vet.

- [ ] **Step 5: Commit**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
git add internal/relay/audit.go internal/relay/revoke.go internal/relay/keystore.go internal/relay/authhttp.go internal/relay/serve.go cmd/woca-relay/serve.go internal/relay/revoke_test.go
git commit -m "relay: revocation cascade and append-only audit log"
```

---

### Task 7: `internal/relayclient` — bridge tunnel client

**Files:**
- Create: internal/relayclient/wire.go
- Create: internal/relayclient/recorder.go
- Create: internal/relayclient/heartbeat.go
- Create: internal/relayclient/client.go
- Test: internal/relayclient/recorder_test.go
- Test: internal/relayclient/heartbeat_test.go
- Test: internal/relayclient/client_test.go
- Test: internal/relayclient/integration_test.go

**Interfaces:**
- Consumes: semantics (NOT imports) of `internal/ee/bridge/tunnel.go:273-336` (`streamRecorder`) and `:194-271` (`execute`/`respond`); `internal/relay` only inside `integration_test.go` (test-only import — `go list -deps ./cmd/woca` does not include test imports of other packages, so `ci/deps-personal.txt` is unaffected; verify with `make check-deps` in Task 9).
- Produces (relied on by Task 8):
  - `const KeyPrefix = "wcr_"` (relayclient's own copy), unexported wire consts `generationHeader`/`statusHeader`/`headersHeader`/`remoteHeader`
  - `type pollPayload struct { ID, Method, Path, Query, Client string; Headers map[string][]string; Body string }`
  - `func newStreamRecorder(pw io.Writer) *streamRecorder` + `Header/WriteHeader/Write/Flush/finish/snapshot/committedStatus/isEventStream`
  - `const heartbeatInterval = 20 * time.Second`, `func newHeartbeatWriter(w io.Writer, ivl time.Duration) *heartbeatWriter` + `arm/Write/stop`
  - `type Config struct { URL, Key string; Handler http.Handler; Workers int; HTTPClient *http.Client; HeartbeatInterval time.Duration }`
  - `type Status struct { Configured, Connected bool; KeyPrefix, Generation string; Revoked bool; LastError string; Served int64 }` (JSON-tagged)
  - `func New(cfg Config, log zerolog.Logger) *Client`, `func (c *Client) Run(ctx context.Context)`, `func (c *Client) Status() Status`
  - Test helpers (same package): `testLogger() zerolog.Logger`, `newFakeRelay(t) *fakeRelay` (+ `offer`, `setPollCode`, `lastRespond`, `polls`)

- [ ] **Step 1: Write the failing tests**

Write `internal/relayclient/recorder_test.go`:

```go
package relayclient

import (
	"io"
	"net/http"
	"testing"
	"time"
)

func TestRecorderFreezesAtFirstWrite(t *testing.T) {
	pr, pw := io.Pipe()
	rec := newStreamRecorder(pw)
	go func() {
		defer rec.finish()
		// The ledger SSE path's awkward order (internal/server/runs.go):
		// a probe flush FIRST, the content type set only afterwards.
		// Freezing at flush would lose the content type and the SPA's
		// onopen check would reject the stream; freezing at first write
		// keeps it (design §6.3).
		rec.Flush()
		rec.Header().Set("Content-Type", "text/event-stream")
		rec.Header().Set("Content-Length", "123") // dropped from snapshots
		rec.Header().Set("Connection", "keep-alive")
		_, _ = rec.Write([]byte("data: x\n\n"))
		_ = pw.Close()
	}()
	<-rec.ready
	snap := rec.snapshot()
	if got := snap["Content-Type"]; len(got) != 1 || got[0] != "text/event-stream" {
		t.Fatalf("content type lost at snapshot: %v", snap)
	}
	if _, ok := snap["Content-Length"]; ok {
		t.Fatalf("Content-Length must be dropped from snapshots: %v", snap)
	}
	if !rec.isEventStream() {
		t.Fatal("isEventStream false for a text/event-stream snapshot")
	}
	if status := rec.committedStatus(); status != http.StatusOK {
		t.Fatalf("implicit status: %d, want 200", status)
	}
	body, _ := io.ReadAll(pr)
	if string(body) != "data: x\n\n" {
		t.Fatalf("body: %q", body)
	}
}

func TestRecorderExplicitStatusFreezesOnce(t *testing.T) {
	pr, pw := io.Pipe()
	rec := newStreamRecorder(pw)
	go func() {
		defer rec.finish()
		rec.Header().Set("X-First", "1")
		rec.WriteHeader(http.StatusTeapot)
		rec.WriteHeader(http.StatusOK) // ignored: status froze at the first call
		_, _ = rec.Write([]byte("chunk"))
		_ = pw.Close()
	}()
	<-rec.ready
	if rec.committedStatus() != http.StatusTeapot {
		t.Fatalf("status: %d, want 418 (first WriteHeader wins)", rec.committedStatus())
	}
	if snap := rec.snapshot(); snap["X-First"][0] != "1" {
		t.Fatalf("snapshot: %v", snap)
	}
	_, _ = io.ReadAll(pr)
}

func TestRecorderEmptyResponse(t *testing.T) {
	// A handler that never writes still releases the uploader via finish.
	pr, pw := io.Pipe()
	rec := newStreamRecorder(pw)
	go func() {
		defer rec.finish()
		_ = pw.Close()
	}()
	select {
	case <-rec.ready:
	case <-time.After(time.Second):
		t.Fatal("finish did not release ready")
	}
	if rec.committedStatus() != http.StatusOK {
		t.Fatal("empty response must commit 200")
	}
	_, _ = io.ReadAll(pr)
}
```

Write `internal/relayclient/heartbeat_test.go`:

```go
package relayclient

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestHeartbeatInjectsPingAfterIdle(t *testing.T) {
	pr, pw := io.Pipe()
	hb := newHeartbeatWriter(pw, 60*time.Millisecond)
	defer hb.stop()
	hb.arm()
	done := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(pr)
		done <- string(data)
	}()
	time.Sleep(200 * time.Millisecond)
	_ = pw.Close()
	got := <-done
	if !strings.Contains(got, ": ping\n\n") {
		t.Fatalf("no heartbeat after idle: %q", got)
	}
	// Pings are standalone comment lines — never torn into data frames
	// (design §6.3 frame safety).
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if line != "" && line != ": ping" {
			t.Fatalf("torn frame line %q in %q", line, got)
		}
	}
}

func TestHeartbeatResetsOnData(t *testing.T) {
	pr, pw := io.Pipe()
	hb := newHeartbeatWriter(pw, 150*time.Millisecond)
	defer hb.stop()
	got := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(pr)
		got <- string(data)
	}()
	hb.arm()
	// Frames flow more often than the interval: no ping may interleave.
	for i := 0; i < 3; i++ {
		if _, err := hb.Write([]byte("data: frame\n\n")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Millisecond)
	}
	_ = pw.Close()
	out := <-got
	if strings.Contains(out, ": ping") {
		t.Fatalf("ping injected despite traffic: %q", out)
	}
	if strings.Count(out, "data: frame") != 3 {
		t.Fatalf("frames mangled: %q", out)
	}
}

func TestHeartbeatUnarmedIsPassThrough(t *testing.T) {
	pr, pw := io.Pipe()
	hb := newHeartbeatWriter(pw, 20*time.Millisecond)
	defer hb.stop()
	got := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(pr)
		got <- string(data)
	}()
	// Non-SSE responses never arm: pure pass-through, zero overhead.
	if _, err := hb.Write([]byte("plain")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	_ = pw.Close()
	if out := <-got; out != "plain" {
		t.Fatalf("unarmed writer altered the stream: %q", out)
	}
}

func TestHeartbeatStopIsIdempotent(t *testing.T) {
	hb := newHeartbeatWriter(io.Discard, time.Second)
	hb.arm()
	hb.stop()
	hb.stop() // must not panic (execute defers stop on every path)
}
```

Write `internal/relayclient/client_test.go`:

```go
package relayclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func testLogger() zerolog.Logger { return zerolog.Nop() }

// fakeRelay is the scriptable relay double: polls answer from scripted
// state, responds are captured for assertions, and a hijack-close mode
// simulates real transport failures (connection reset mid-poll).
type fakeRelay struct {
	t     *testing.T
	srv   *httptest.Server
	polls atomic.Int64

	hijackClose atomic.Bool

	mu       sync.Mutex
	payload  *pollPayload
	pollCode int
	responds []capturedRespond
}

type capturedRespond struct {
	id      string
	status  string
	headers map[string][]string
	body    string
	gen     string
}

func newFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()
	f := &fakeRelay{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tunnel/poll":
			if f.hijackClose.Load() {
				// Kill the TCP connection without any HTTP response:
				// hc.Do sees a genuine transport error (unexpected EOF),
				// which is what regeneration keys off — an answered HTTP
				// 5xx deliberately does NOT rotate the generation (the
				// relay is alive and still holds this generation's 45s
				// lease; a fresh one would 409-collide with it).
				hj, ok := w.(http.Hijacker)
				if !ok {
					http.Error(w, "hijack unsupported", http.StatusInternalServerError)
					return
				}
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
			f.polls.Add(1)
			f.mu.Lock()
			code := f.pollCode
			p := f.payload
			f.payload = nil
			f.mu.Unlock()
			switch {
			case code != 0:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				if code == http.StatusGone {
					_, _ = w.Write([]byte(`{"code":"revoked"}`))
				}
			case p != nil:
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(p)
			default:
				w.WriteHeader(http.StatusNoContent)
			}
		case strings.HasPrefix(r.URL.Path, "/tunnel/respond/"):
			body, _ := io.ReadAll(r.Body)
			var headers map[string][]string
			raw, _ := base64.StdEncoding.DecodeString(r.Header.Get(headersHeader))
			_ = json.Unmarshal(raw, &headers)
			f.mu.Lock()
			f.responds = append(f.responds, capturedRespond{
				id:      strings.TrimPrefix(r.URL.Path, "/tunnel/respond/"),
				status:  r.Header.Get(statusHeader),
				headers: headers,
				body:    string(body),
				gen:     r.Header.Get(generationHeader),
			})
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRelay) offer(p pollPayload) {
	f.mu.Lock()
	f.payload = &p
	f.mu.Unlock()
}

func (f *fakeRelay) setPollCode(code int) {
	f.mu.Lock()
	f.pollCode = code
	f.mu.Unlock()
}

// setHijackClose toggles the transport-failure simulation.
func (f *fakeRelay) setHijackClose(on bool) {
	f.hijackClose.Store(on)
}

func (f *fakeRelay) respondCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.responds)
}

func (f *fakeRelay) lastRespond() capturedRespond {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.responds) == 0 {
		f.t.Fatal("no respond captured")
	}
	return f.responds[len(f.responds)-1]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestClientRoundTrip(t *testing.T) {
	relay := newFakeRelay(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Header hygiene (design §6.4): the forged signalling header is
		// stripped; the remote marker is injected by this process, after
		// the strip.
		if got := r.Header.Get("X-Woca-Relay-Generation"); got != "" {
			t.Errorf("signalling header reached the handler: %q", got)
		}
		if got := r.Header.Get(remoteHeader); got != "1" {
			t.Errorf("remote marker missing: %q", got)
		}
		if r.RemoteAddr != "203.0.113.7:5555" {
			t.Errorf("RemoteAddr %q (the real client IP must propagate)", r.RemoteAddr)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("ordinary header lost: Accept=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	})
	c := New(Config{URL: relay.srv.URL, Key: "wcr_test", Handler: handler, Workers: 1}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	relay.offer(pollPayload{
		ID:     "req-1",
		Method: "GET",
		Path:   "/api/sessions",
		Client: "203.0.113.7:5555",
		Headers: map[string][]string{
			"X-Woca-Relay-Generation": {"forged"},
			"Accept":                  {"application/json"},
		},
	})
	waitFor(t, "respond upload", func() bool { return relay.respondCount() > 0 })
	got := relay.lastRespond()
	if got.id != "req-1" || got.status != "200" {
		t.Fatalf("respond: %+v", got)
	}
	if ct := got.headers["Content-Type"]; len(ct) != 1 || ct[0] != "application/json" {
		t.Fatalf("respond headers: %v", got.headers)
	}
	if !strings.Contains(got.body, `"path":"/api/sessions"`) {
		t.Fatalf("respond body: %q", got.body)
	}
	if got.gen == "" {
		t.Fatal("respond carried no generation header")
	}
}

func TestClientTerminalOnRevoked(t *testing.T) {
	relay := newFakeRelay(t)
	relay.setPollCode(http.StatusGone)
	c := New(Config{URL: relay.srv.URL, Key: "wcr_test", Handler: http.NotFoundHandler(), Workers: 2}, testLogger())
	done := make(chan struct{})
	go func() {
		c.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("410 revoked did not terminate the tunnel")
	}
	st := c.Status()
	if !st.Revoked || st.LastError != "key revoked" {
		t.Fatalf("status: %+v (the status page reads 'key revoked')", st)
	}
	// Terminal means terminal: no more polls, ever (design §6.5).
	polls := relay.polls.Load()
	time.Sleep(200 * time.Millisecond)
	if relay.polls.Load() != polls {
		t.Fatal("terminal state kept polling")
	}
}

func TestClientTerminalOnUnauthorized(t *testing.T) {
	relay := newFakeRelay(t)
	relay.setPollCode(http.StatusUnauthorized)
	c := New(Config{URL: relay.srv.URL, Key: "wcr_test", Handler: http.NotFoundHandler(), Workers: 1}, testLogger())
	done := make(chan struct{})
	go func() {
		c.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("401 did not terminate the tunnel")
	}
	if st := c.Status(); st.Revoked {
		t.Fatalf("401 is terminal but not 'revoked': %+v", st)
	}
}

func TestClientReconnectsWithNewGeneration(t *testing.T) {
	relay := newFakeRelay(t)
	// Regeneration is triggered by a REAL transport failure (connection
	// reset mid-poll), never by an answered HTTP 5xx: an answered 5xx
	// means the relay is alive and still holds this generation's 45s
	// lease, so rotating there would 409-collide the fresh generation
	// with the bridge's own leased one — up to 45s dark (design §6.2's
	// first-come-first-served rule). The implementation regenerates only
	// on hc.Do errors; this test therefore uses the hijack-close mode.
	relay.setHijackClose(true)
	c := New(Config{URL: relay.srv.URL, Key: "wcr_test", Handler: http.NotFoundHandler(), Workers: 1}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	first := c.generation()
	waitFor(t, "generation rotation", func() bool { return c.generation() != first })
	// Once the transport heals, the SAME (rotated) generation resumes
	// polling — retry, not terminal.
	relay.setHijackClose(false)
	waitFor(t, "recovery polls", func() bool { return relay.polls.Load() > 0 })
	if st := c.Status(); st.Revoked {
		t.Fatal("transport failure is retryable, not terminal")
	}
}

func TestClient5xxKeepsGeneration(t *testing.T) {
	relay := newFakeRelay(t)
	// An answered HTTP 500 is a plain retry: the relay is alive and holds
	// the lease — the generation must NOT rotate (it would 409-collide
	// with the bridge's own live generation for up to 45s).
	relay.setPollCode(http.StatusInternalServerError)
	c := New(Config{URL: relay.srv.URL, Key: "wcr_test", Handler: http.NotFoundHandler(), Workers: 1}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	first := c.generation()
	waitFor(t, "retry polls", func() bool { return relay.polls.Load() >= 2 })
	if got := c.generation(); got != first {
		t.Fatalf("generation rotated on HTTP 5xx (%s → %s); rotation is transport-error-only", first, got)
	}
	if st := c.Status(); st.Revoked {
		t.Fatal("5xx is retryable, not terminal")
	}
}

func TestClient204KeepsPolling(t *testing.T) {
	relay := newFakeRelay(t) // default: 204
	c := New(Config{URL: relay.srv.URL, Key: "wcr_test", Handler: http.NotFoundHandler(), Workers: 1}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitFor(t, "repeat polls", func() bool { return relay.polls.Load() >= 3 })
	if st := c.Status(); !st.Connected || st.Revoked {
		t.Fatalf("status during idle polling: %+v", st)
	}
}

func TestBackoffBounds(t *testing.T) {
	for i := 0; i < 200; i++ {
		if d := backoff(0); d < 800*time.Millisecond || d > 1200*time.Millisecond {
			t.Fatalf("attempt 0: %v outside ±20%% of 1s", d)
		}
		if d := backoff(100); d < 48*time.Second || d > 72*time.Second {
			t.Fatalf("capped: %v outside ±20%% of 60s", d)
		}
	}
	if d := backoff(3); d < 6*time.Second+400*time.Millisecond || d > 9*time.Second+600*time.Millisecond {
		t.Fatalf("attempt 3: %v outside ±20%% of 8s", d)
	}
}
```

Write `internal/relayclient/integration_test.go`:

```go
package relayclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/projanvil/woca/internal/relay"
)

// TestFullChain wires the REAL relay (internal/relay — test-only import)
// and the real tunnel client around a stub bridge handler: the §10
// integration shape — round trip, SSE fidelity, mandatory heartbeat.
func TestFullChain(t *testing.T) {
	ks, err := relay.OpenKeyStore(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ks.Close() })
	_, raw, err := ks.CreateKey(t.Context(), "home-mac")
	if err != nil {
		t.Fatal(err)
	}
	rl := relay.NewRelay(ks, fstest.MapFS{"index.html": {Data: []byte("app")}}, false, testLogger())
	srv := httptest.NewServer(rl.Handler())
	t.Cleanup(srv.Close)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/echo":
			fmt.Fprintf(w, "echo:%s", r.URL.Query().Get("q"))
		case "/api/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: start\n\n")
			// 350ms of silence: the 100ms test heartbeat must fill it.
			time.Sleep(350 * time.Millisecond)
			fmt.Fprint(w, "data: end\n\n")
		default:
			http.NotFound(w, r)
		}
	})
	c := New(Config{
		URL:               srv.URL,
		Key:               raw,
		Handler:           handler,
		Workers:           2,
		HeartbeatInterval: 100 * time.Millisecond,
	}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	resp, err := client.Post(srv.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"api_key":"`+raw+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", resp.StatusCode)
	}

	// Round trip through login → park → poll → in-process handler →
	// respond → client.
	var body string
	waitFor(t, "echo round trip", func() bool {
		resp, err := client.Get(srv.URL + "/api/echo?q=hi")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		data, _ := io.ReadAll(resp.Body)
		body = string(data)
		return true
	})
	if body != "echo:hi" {
		t.Fatalf("echo: %q", body)
	}

	// SSE: frames pass through and the idle gap carries the mandatory
	// heartbeat ping (design §6.3 — without it idle SSE dies in
	// intermediary 60-100s windows).
	resp, err = client.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	out := string(data)
	for _, want := range []string{"data: start", ": ping", "data: end"} {
		if !strings.Contains(out, want) {
			t.Fatalf("SSE stream %q missing %q", out, want)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
go test ./internal/relayclient/ -count=1
```

Expected failure: `internal/relayclient/recorder_test.go:6:14: undefined: newStreamRecorder` and siblings (`newHeartbeatWriter`, `New`, `Config`, `pollPayload`, `generationHeader`, `statusHeader`, `headersHeader`, `remoteHeader`, `backoff`).

- [ ] **Step 3: Write minimal implementation**

Write `internal/relayclient/wire.go`:

```go
// Package relayclient is the bridge side of the personal remote relay
// (design 2026-09-17-personal-relay-design §3.2): an outbound long-poll
// tunnel client compiled into the personal woca binary. It executes
// parked client requests against the in-process server handler — the same
// code path as local web traffic, no TCP loopback — and streams answers
// back chunk by chunk.
//
// The wire vocabulary below mirrors internal/relay deliberately (the two
// packages stay independently compilable; semantics copied from the
// enterprise internal/ee/bridge tunnel, code rewritten — nothing is
// imported from internal/ee).
package relayclient

// KeyPrefix is relayclient's own copy of the relay key prefix (used by
// the CLI to validate input without importing the relay server package).
const KeyPrefix = "wcr_"

// Tunnel wire headers (mirror internal/relay).
const (
	generationHeader = "X-Woca-Relay-Generation"
	statusHeader     = "X-Woca-Relay-Status"
	headersHeader    = "X-Woca-Relay-Headers"
	remoteHeader     = "X-Woca-Relay-Remote"
)

// pollPayload mirrors the relay's parked-request wire shape (the 200 body
// of /tunnel/poll).
type pollPayload struct {
	ID      string              `json:"id"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Query   string              `json:"query"`
	Client  string              `json:"client"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"` // base64 standard encoding
}
```

Write `internal/relayclient/recorder.go`:

```go
package relayclient

import (
	"io"
	"net/http"
	"strings"
	"sync"
)

// streamRecorder captures a handler's response as a stream (design §6.3;
// a same-semantics rewrite of the enterprise streamRecorder — code in
// internal/ee/bridge/tunnel.go:273-336, NOT importable): status and
// headers freeze at the first Write/WriteHeader — exactly when net/http
// would commit them — NOT at Flush, because woca's ledger SSE path sets
// Content-Type: text/event-stream only after a probe flush; freezing at
// flush would drop the content type and the SPA's onopen check would
// reject the stream. Flush is a no-op boundary hint: every pipe write
// already becomes one transport chunk.
type streamRecorder struct {
	pw     io.Writer
	header http.Header
	status int

	mu        sync.Mutex
	ready     chan struct{}
	markReady func()
}

func newStreamRecorder(pw io.Writer) *streamRecorder {
	r := &streamRecorder{pw: pw, header: http.Header{}, ready: make(chan struct{})}
	r.markReady = sync.OnceFunc(func() { close(r.ready) })
	return r
}

func (r *streamRecorder) Header() http.Header { return r.header }

func (r *streamRecorder) WriteHeader(code int) {
	r.mu.Lock()
	if r.status == 0 {
		r.status = code
	}
	r.mu.Unlock()
	r.markReady()
}

func (r *streamRecorder) Write(b []byte) (int, error) {
	r.markReady()
	return r.pw.Write(b)
}

// Flush is a no-op: pipe writes already chunk the stream (design §6.3).
func (r *streamRecorder) Flush() {}

// finish releases waiters when the handler returns without writing.
func (r *streamRecorder) finish() { r.markReady() }

// snapshot copies the frozen headers, dropping Content-Length — a
// streamed answer has no fixed length (design §6.3).
func (r *streamRecorder) snapshot() map[string][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string][]string, len(r.header))
	for k, vs := range r.header {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// committedStatus returns the frozen status (implicit 200).
func (r *streamRecorder) committedStatus() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// isEventStream reports whether the frozen headers declare SSE (the
// heartbeat wraps only these).
func (r *streamRecorder) isEventStream() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.HasPrefix(r.header.Get("Content-Type"), "text/event-stream")
}
```

Write `internal/relayclient/heartbeat.go`:

```go
package relayclient

import (
	"io"
	"sync"
	"time"
)

// heartbeatInterval is the idle window after which an SSE comment ping is
// injected (design §6.3: 20s — intermediary idle windows run 60-100s).
const heartbeatInterval = 20 * time.Second

// pingLine is a valid SSE comment frame; single-line writes cannot tear
// data frames (design §6.3: every woca SSE frame is one Write and the
// pipe serializes writers).
const pingLine = ": ping\n\n"

// heartbeatWriter wraps the respond pipe for text/event-stream responses:
// the configured idle window injects one ping line (design §6.3 —
// MANDATORY, not optional; without it idle SSE dies in the relay front's
// proxy window). It also keeps the bridge→relay respond upload connection
// alive.
type heartbeatWriter struct {
	w    io.Writer
	ivl  time.Duration
	done chan struct{}

	mu    sync.Mutex
	timer *time.Timer
}

func newHeartbeatWriter(w io.Writer, ivl time.Duration) *heartbeatWriter {
	return &heartbeatWriter{w: w, ivl: ivl, done: make(chan struct{})}
}

// arm starts the idle timer; unarmed writers pass data through untouched
// (non-SSE responses never inject pings).
func (h *heartbeatWriter) arm() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.timer != nil {
		return
	}
	h.timer = time.AfterFunc(h.ivl, h.ping)
}

func (h *heartbeatWriter) ping() {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.done:
		return
	default:
	}
	// A blocked pipe write is tolerated (design §6.3): the execute
	// cleanup closes the pipe on cancellation, which errors this write
	// and stops re-arming.
	if _, err := h.w.Write([]byte(pingLine)); err != nil {
		return
	}
	h.timer.Reset(h.ivl)
}

// Write passes data through, resetting the idle timer.
func (h *heartbeatWriter) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, err := h.w.Write(p)
	if h.timer != nil {
		h.timer.Reset(h.ivl)
	}
	return n, err
}

// stop ends the heartbeat; idempotent (execute defers it on every path).
func (h *heartbeatWriter) stop() {
	h.mu.Lock()
	if h.timer != nil {
		h.timer.Stop()
	}
	h.mu.Unlock()
	select {
	case <-h.done:
	default:
		close(h.done)
	}
}
```

Write `internal/relayclient/client.go`:

```go
package relayclient

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// Tunnel cadences (design §3.2/§6.5).
const (
	pollWait       = 25 * time.Second
	backoffMin     = time.Second
	backoffMax     = 60 * time.Second
	defaultWorkers = 4
)

// Config wires the tunnel client.
type Config struct {
	// URL is the relay base URL (https://relay.example.com).
	URL string
	// Key is the wcr_ rendezvous key (the same key clients log in with).
	Key string
	// Handler executes parked requests in process — server.Handler().
	Handler http.Handler
	// Workers is the concurrent poller count (default 4, design §3.2;
	// each parked request occupies one worker until its answer completes,
	// so SSE streams must not starve plain requests).
	Workers int
	// HTTPClient overrides the transport (tests); nil uses a
	// streaming-safe default.
	HTTPClient *http.Client
	// HeartbeatInterval overrides the 20s SSE heartbeat (tests).
	HeartbeatInterval time.Duration
}

// Status is the /api/relay/status snapshot (design §3.2).
type Status struct {
	Configured bool   `json:"configured"`
	Connected  bool   `json:"connected"`
	KeyPrefix  string `json:"key_prefix,omitempty"`
	Generation string `json:"generation,omitempty"`
	Revoked    bool   `json:"revoked"`
	LastError  string `json:"last_error,omitempty"`
	Served     int64  `json:"served"`
}

// Client is the outbound tunnel: worker goroutines long-poll the relay,
// execute parked requests against Handler, and stream responses back.
type Client struct {
	cfg Config
	log zerolog.Logger
	hc  *http.Client

	genMu sync.Mutex
	gen   string

	live   atomic.Int64
	served atomic.Int64

	stMu      sync.Mutex
	lastError string
	revoked   bool

	term       context.Context
	termCancel context.CancelFunc
}

// New builds the tunnel client. The relay URL is normalized by trimming
// trailing slashes: endpoints are joined as URL+"/tunnel/poll", and a
// stored "https://relay.example.com/" would otherwise produce
// "//tunnel/poll" — which net/http ServeMux answers with a 301 redirect
// that breaks POST /tunnel/respond (a redirected POST re-issues as GET).
func New(cfg Config, log zerolog.Logger) *Client {
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	hc := cfg.HTTPClient
	if hc == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = 0 // streams answer late by design
		hc = &http.Client{Timeout: 0, Transport: transport}
	}
	return &Client{cfg: cfg, log: log, hc: hc}
}

// Run drives the worker pool until ctx ends or a terminal state (key
// revoked/unknown) stops the tunnel (design §6.5).
func (c *Client) Run(ctx context.Context) {
	if c.cfg.Handler == nil || c.cfg.URL == "" || c.cfg.Key == "" {
		return
	}
	c.term, c.termCancel = context.WithCancel(ctx)
	defer c.termCancel()
	c.regenerate()
	workers := c.cfg.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() { c.worker(c.term) })
	}
	wg.Wait()
}

// Status snapshots the tunnel state.
func (c *Client) Status() Status {
	c.stMu.Lock()
	defer c.stMu.Unlock()
	prefix := c.cfg.Key
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return Status{
		Configured: c.cfg.URL != "" && c.cfg.Key != "",
		Connected:  c.live.Load() > 0 && !c.revoked,
		KeyPrefix:  prefix,
		Generation: c.generation(),
		Revoked:    c.revoked,
		LastError:  c.lastError,
		Served:     c.served.Load(),
	}
}

// generation is the current generation token.
func (c *Client) generation() string {
	c.genMu.Lock()
	defer c.genMu.Unlock()
	return c.gen
}

// regenerate rotates the generation — every reconnect cycle (design
// §6.2/§6.5: the relay's 45s lease lets the old generation yield).
func (c *Client) regenerate() {
	var raw [16]byte
	if _, err := crand.Read(raw[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	c.genMu.Lock()
	c.gen = base64.RawURLEncoding.EncodeToString(raw[:])
	c.genMu.Unlock()
}

// setTerminal records a terminal failure and stops every worker —
// terminal, never a backoff retry (design §5.1/§6.5).
func (c *Client) setTerminal(revoked bool, msg string) {
	c.stMu.Lock()
	c.revoked = revoked
	c.lastError = msg
	c.stMu.Unlock()
	c.termCancel()
}

func (c *Client) setError(msg string) {
	c.stMu.Lock()
	c.lastError = msg
	c.stMu.Unlock()
}

// pollOutcome classifies one poll cycle.
type pollOutcome int

const (
	outcomeOK       pollOutcome = iota // picked up work
	outcomeTimeout                     // 204 — immediate re-poll
	outcomeRetry                       // network/5xx — backoff
	outcomeConflict                    // 409 — another generation lives
	outcomeTerminal                    // 401/410 — stop reconnecting
)

func (c *Client) worker(ctx context.Context) {
	c.live.Add(1)
	defer c.live.Add(-1)
	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		switch c.pollOnce(ctx) {
		case outcomeOK, outcomeTimeout:
			attempt = 0
		case outcomeRetry:
			if !sleepCtx(ctx, backoff(attempt)) {
				return
			}
			attempt++
		case outcomeConflict:
			// Our generation loses while the previous one is still live:
			// back off; the relay's 45s lease evicts the old one
			// (design §6.2).
			if !sleepCtx(ctx, backoff(attempt)) {
				return
			}
			attempt++
		case outcomeTerminal:
			return
		}
	}
}

// backoff is 1s doubling to 60s with ±20% jitter (design §6.5).
func backoff(attempt int) time.Duration {
	d := backoffMin << min(attempt, 6)
	if d > backoffMax {
		d = backoffMax
	}
	return time.Duration(float64(d) * (0.8 + 0.4*mrand.Float64()))
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// pollOnce performs one long poll; a picked-up request runs on its own
// goroutine so SSE streams never starve the worker pool.
func (c *Client) pollOnce(ctx context.Context) pollOutcome {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.cfg.URL+"/tunnel/poll?wait="+pollWait.String(), nil)
	if err != nil {
		c.setTerminal(false, "invalid relay URL")
		return outcomeTerminal
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Key)
	req.Header.Set(generationHeader, c.generation())
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return outcomeTerminal // shutdown, not a failure
		}
		c.setError("poll: " + err.Error())
		c.regenerate() // a fresh generation per reconnect (design §6.5)
		return outcomeRetry
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		var payload pollPayload
		if err := json.NewDecoder(io.LimitReader(resp.Body, (8<<20)+1)).Decode(&payload); err != nil {
			c.setError("poll decode: " + err.Error())
			return outcomeRetry
		}
		c.served.Add(1)
		go c.execute(ctx, payload)
		return outcomeOK
	case http.StatusNoContent:
		return outcomeTimeout
	case http.StatusConflict:
		return outcomeConflict
	case http.StatusGone, http.StatusUnauthorized:
		// 410 {"code":"revoked"} or 401: terminal — never re-poll
		// (design §5.1/§6.5).
		c.setTerminal(resp.StatusCode == http.StatusGone, terminalMessage(resp))
		return outcomeTerminal
	default:
		c.setError(fmt.Sprintf("poll: unexpected status %d", resp.StatusCode))
		return outcomeRetry
	}
}

// terminalMessage extracts the relay's error code for the status surface.
func terminalMessage(resp *http.Response) string {
	var body struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&body)
	if body.Code == "revoked" {
		return "key revoked"
	}
	return fmt.Sprintf("tunnel authentication failed (HTTP %d)", resp.StatusCode)
}

// execute runs one parked request against the in-process handler and
// streams the answer back (design §3.2/§6.3/§6.4).
func (c *Client) execute(ctx context.Context, payload pollPayload) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error().Any("panic", r).Str("path", payload.Path).Msg("relayclient: tunneled request panicked")
		}
	}()
	var body []byte
	if payload.Body != "" {
		var err error
		if body, err = base64.StdEncoding.DecodeString(payload.Body); err != nil {
			c.setError("payload body: " + err.Error())
			return
		}
	}
	target := payload.Path
	if payload.Query != "" {
		target += "?" + payload.Query
	}
	req, err := http.NewRequestWithContext(ctx, payload.Method, "http://relay.local"+target, bytes.NewReader(body))
	if err != nil {
		c.setError("payload request: " + err.Error())
		return
	}
	for k, vs := range payload.Headers {
		// Relay signalling vocabulary never reaches the handler (design
		// §6.4 — a client-forged X-Woca-* header dies here; the superset
		// covers X-Woca-Relay-*).
		if strings.HasPrefix(k, "X-Woca-") {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// Injected AFTER the strip: neither relay nor client can forge the
	// remote marker (design §6.4 — local audit marks remote origin).
	req.Header.Set(remoteHeader, "1")
	if payload.Client != "" {
		req.RemoteAddr = payload.Client
	} else {
		req.RemoteAddr = "relay:0"
	}

	ivl := c.cfg.HeartbeatInterval
	if ivl <= 0 {
		ivl = heartbeatInterval
	}
	pr, pw := io.Pipe()
	hb := newHeartbeatWriter(pw, ivl)
	rec := newStreamRecorder(hb)
	go func() {
		defer rec.finish()
		c.cfg.Handler.ServeHTTP(rec, req)
		_ = pw.Close()
	}()
	select {
	case <-rec.ready:
	case <-ctx.Done():
		_ = pr.CloseWithError(context.Canceled)
		hb.stop()
		return
	}
	if rec.isEventStream() {
		hb.arm()
	}
	c.respond(ctx, payload, rec, pr, hb)
}

// respond uploads the recorded response: frozen status + headers first,
// then the body chunk by chunk as the handler produces it (design §6.3 —
// the Go transport chunks the request body automatically).
func (c *Client) respond(ctx context.Context, payload pollPayload, rec *streamRecorder, pr *io.PipeReader, hb *heartbeatWriter) {
	defer hb.stop()
	rawHeaders, err := json.Marshal(rec.snapshot())
	if err != nil {
		_ = pr.CloseWithError(err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.URL+"/tunnel/respond/"+payload.ID, pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Key)
	req.Header.Set(generationHeader, c.generation())
	req.Header.Set(statusHeader, fmt.Sprintf("%d", rec.committedStatus()))
	req.Header.Set(headersHeader, base64.StdEncoding.EncodeToString(rawHeaders))
	resp, err := c.hc.Do(req)
	if err != nil {
		// Unstick the handler goroutine: further pipe writes fail fast
		// (design §6.5 — relay restart mid-respond).
		_ = pr.CloseWithError(err)
		c.setError("respond: " + err.Error())
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body) // drain for connection reuse
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound {
		// Client gone or a superseded generation: stop the handler
		// cleanly (the relay's 410/404 semantics, design §6.2).
		_ = pr.CloseWithError(fmt.Errorf("relayclient: respond rejected (HTTP %d)", resp.StatusCode))
		return
	}
	_ = pr.CloseWithError(errors.New("relayclient: respond complete"))
}
```

- [ ] **Step 4: Run tests to verify they pass**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
go test ./internal/relayclient/ -count=1
go test ./internal/relayclient/ -race -count=1
```

Expected: `ok  	github.com/projanvil/woca/internal/relayclient` (both runs) — recorder freeze semantics, heartbeat injection/reset/passthrough, terminal states, generation rotation, backoff bounds, and the full-chain integration (echo + SSE with `: ping`) all PASS.

- [ ] **Step 5: Commit**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
git add internal/relayclient/
git commit -m "relayclient: bridge tunnel client with streamRecorder rewrite and SSE heartbeat"
```

---

### Task 8: `woca relay` CLI + local status + serve wiring

**Files:**
- Create: internal/relayclient/persist.go
- Create: internal/relayclient/service.go
- Modify: internal/settings/settings.go:68-79 (add `SecretRelayKey`)
- Modify: internal/settings/settings.go:84-120 (add `RelayURL` field to `Settings` after `ModelWhitelist`, :117)
- Modify: internal/settings/settings.go:367-504 (`applyPairs`: add `relay_url`)
- Modify: internal/settings/settings.go:542-555 (`Save`: append `relay_url` field)
- Modify: internal/settings/keys.go:11-62 (add `KeyRelayURL`)
- Modify: internal/settings/keys.go:231-422 (`Apply`: add `relay-url` case before `default` at :418)
- Modify: internal/settings/keys.go:484-568 (`KeyValue`: add `relay-url` case)
- Modify: cmd/woca/main.go:16-59 (imports: `authn`, `relayclient`)
- Modify: cmd/woca/main.go:121-128 (serve `Edition` wiring + `newRelayCmd` registration)
- Modify: cmd/woca/main.go (append after `newConfigCmd` which ends at :326: `newRelayCmd`, `fetchRelayStatus`, `printRelayStatus`)
- Test: internal/relayclient/persist_test.go
- Test: internal/relayclient/service_test.go

**Interfaces:**
- Consumes (Task 7): `relayclient.Config`, `Client`, `Status`, `New`, `KeyPrefix`; repo seams: `servefront.Edition{Handler, OnSettings, Start, Routes}` (internal/servefront/serve.go:96,101,141,91 — `Handler` receives the fully-built server handler at servefront/serve.go:502-504, `OnSettings` receives `ap.Settings` at :424-426, `Start` runs in a goroutine at :515-517, `Routes` mounts via `server.Config.Extras` at :468-472 → server.go:351-353), `settings.Open(target)` (:251), `(*Store).SetSecret/GetSecret` (:623,:649), `(*Store).Clear` (:592), `settings.Apply` (keys.go:232), `woca config`'s `open(cmd)` --db/WOCA_DB pattern (cmd/woca/main.go:244-257), `webgui.DefaultAddr` (servefront/serve.go:212 uses it for the serve default).
- Produces:
  - `const EnvRelayURL = "WOCA_RELAY_URL"`, `EnvRelayKey = "WOCA_RELAY_KEY"`
  - `func ResolveConfig(st *settings.Store) (url, key string)`, `func SaveConfig(st, url, key) error`, `func ClearConfig(st) error`
  - `func NewService() *Service`, `func (s *Service) SetHandler(h http.Handler)`, `func (s *Service) SetSettings(st *settings.Store)`, `func (s *Service) Run(ctx, st, log)`, `func (s *Service) Disconnect()`, `func (s *Service) Status() Status`, `func (s *Service) StatusHandler() http.Handler`, `func (s *Service) DisconnectHandler() http.Handler`
  - settings: `settings.SecretRelayKey`, `settings.KeyRelayURL`, `Settings.RelayURL`
  - CLI: `woca relay connect --url --key`, `woca relay status [--server]`, `woca relay disconnect [--server]`; local APIs `GET /api/relay/status`, `POST /api/relay/disconnect`

- [ ] **Step 1: Write the failing tests**

Write `internal/relayclient/persist_test.go`:

```go
package relayclient

import (
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/settings"
)

func openTestSettings(t *testing.T) *settings.Store {
	t.Helper()
	st, err := settings.Open(filepath.Join(t.TempDir(), "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("settings store disabled")
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestSaveResolveClear(t *testing.T) {
	st := openTestSettings(t)
	if err := SaveConfig(st, "https://relay.example.com", "wcr_secret"); err != nil {
		t.Fatal(err)
	}
	url, key := ResolveConfig(st)
	if url != "https://relay.example.com" || key != "wcr_secret" {
		t.Fatalf("resolved %q / %q", url, key)
	}
	// The URL is a plain settings row; the key is encrypted-only
	// (settings secrets, design §7.1 — never plaintext on disk).
	if got := settings.KeyValue(st.Load(), settings.KeyRelayURL); got != "https://relay.example.com" {
		t.Fatalf("relay url row: %q", got)
	}
	if !st.HasSecret(settings.SecretRelayKey) {
		t.Fatal("relay key missing from the secrets table")
	}
	if err := ClearConfig(st); err != nil {
		t.Fatal(err)
	}
	url, key = ResolveConfig(st)
	if url != "" || key != "" {
		t.Fatalf("after clear: %q / %q", url, key)
	}
	if st.HasSecret(settings.SecretRelayKey) {
		t.Fatal("clear left the relay key behind")
	}
}

func TestEnvOverridesSettings(t *testing.T) {
	st := openTestSettings(t)
	if err := SaveConfig(st, "https://stored.example.com", "wcr_stored"); err != nil {
		t.Fatal(err)
	}
	// Container first-boot / headless deployments use the env pair
	// (design §3.2); env wins over persisted values.
	t.Setenv(EnvRelayURL, "https://env.example.com")
	t.Setenv(EnvRelayKey, "wcr_env")
	url, key := ResolveConfig(st)
	if url != "https://env.example.com" || key != "wcr_env" {
		t.Fatalf("env precedence broken: %q / %q", url, key)
	}
}

func TestResolveNilStore(t *testing.T) {
	// A nil store (settings disabled) still honors the environment.
	t.Setenv(EnvRelayURL, "https://env.example.com")
	url, key := ResolveConfig(nil)
	if url != "https://env.example.com" || key != "" {
		t.Fatalf("nil store: %q / %q", url, key)
	}
}

func TestTrailingSlashCanonicalized(t *testing.T) {
	st := openTestSettings(t)
	// A trailing slash must never persist: endpoints join as
	// URL+"/tunnel/poll" and "//tunnel/poll" breaks POST respond through
	// ServeMux redirects.
	if err := SaveConfig(st, "https://relay.example.com/", "wcr_secret"); err != nil {
		t.Fatal(err)
	}
	url, _ := ResolveConfig(st)
	if url != "https://relay.example.com" {
		t.Fatalf("stored url %q, want canonical no-trailing-slash form", url)
	}
	// The env path is canonicalized too.
	t.Setenv(EnvRelayURL, "https://env.example.com//")
	url, _ = ResolveConfig(st)
	if url != "https://env.example.com" {
		t.Fatalf("env url %q, want canonical form", url)
	}
}
```

Write `internal/relayclient/service_test.go`:

```go
package relayclient

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestServiceRunStartsAndStopsTunnel(t *testing.T) {
	relay := newFakeRelay(t)
	st := openTestSettings(t)
	if err := SaveConfig(st, relay.srv.URL, "wcr_test"); err != nil {
		t.Fatal(err)
	}
	svc := NewService()
	svc.SetSettings(st)
	svc.SetHandler(http.NotFoundHandler())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx, st, testLogger())

	waitFor(t, "first poll", func() bool { return relay.polls.Load() > 0 })
	if got := svc.Status(); !got.Connected || !got.Configured || got.KeyPrefix != "wcr_test" {
		t.Fatalf("status: %+v", got)
	}
	svc.Disconnect()
	waitFor(t, "disconnect", func() bool { return !svc.Status().Connected })
}

func TestServiceStatusWithoutConfig(t *testing.T) {
	svc := NewService()
	if got := svc.Status(); got.Configured || got.Connected {
		t.Fatalf("zero-config status: %+v", got)
	}
}

func TestServiceRunWithoutConfigIsNoop(t *testing.T) {
	st := openTestSettings(t)
	svc := NewService()
	svc.SetSettings(st)
	svc.SetHandler(http.NotFoundHandler())
	done := make(chan struct{})
	go func() {
		svc.Run(context.Background(), st, testLogger())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("unconfigured Run did not return (must be a no-op)")
	}
}

func TestServiceStatusHandler(t *testing.T) {
	st := openTestSettings(t)
	if err := SaveConfig(st, "https://relay.example.com", "wcr_test"); err != nil {
		t.Fatal(err)
	}
	svc := NewService()
	svc.SetSettings(st)
	rec := newRecorder()
	svc.StatusHandler().ServeHTTP(rec, newGetRequest("/api/relay/status"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status handler: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"configured":true`, `"key_prefix":"wcr_test"`} {
		if !contains(body, want) {
			t.Fatalf("status body %s missing %s", body, want)
		}
	}
}
```

(Note: `newRecorder`/`newGetRequest`/`contains` are thin aliases the test file defines locally to avoid extra imports:

```go
func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }
func newGetRequest(path string) *http.Request { return httptest.NewRequest(http.MethodGet, path, nil) }
func contains(s, sub string) bool             { return strings.Contains(s, sub) }
```

with `"net/http/httptest"` and `"strings"` added to `service_test.go`'s imports.)

- [ ] **Step 2: Run tests to verify they fail**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
go test ./internal/relayclient/ -run 'TestSave|TestEnv|TestResolve|TestService' -count=1
```

Expected failure: compile errors — `undefined: SaveConfig`, `undefined: ResolveConfig`, `undefined: ClearConfig`, `undefined: EnvRelayURL`, `undefined: NewService`, `undefined: settings.SecretRelayKey`, `undefined: settings.KeyRelayURL`.

- [ ] **Step 3: Write minimal implementation**

Write `internal/relayclient/persist.go`:

```go
package relayclient

import (
	"os"
	"strings"

	"github.com/projanvil/woca/internal/settings"
)

// Environment equivalents of `woca relay connect` (design §3.2 —
// container first-boot / headless deployments). Environment wins over
// persisted settings.
const (
	EnvRelayURL = "WOCA_RELAY_URL"
	EnvRelayKey = "WOCA_RELAY_KEY"
)

// ResolveConfig reads the relay configuration: environment first, then
// the settings store (URL is a plain settings row; the key rides the
// encrypted secrets table — never plaintext on disk, design §7.1).
func ResolveConfig(st *settings.Store) (url, key string) {
	url = os.Getenv(EnvRelayURL)
	key = os.Getenv(EnvRelayKey)
	if url != "" && key != "" {
		return url, key
	}
	if st == nil {
		return url, key
	}
	if url == "" {
		url = st.Load().RelayURL
	}
	if key == "" {
		key = st.GetSecret(settings.SecretRelayKey)
	}
	// Canonical form: no trailing slashes (endpoints join as
	// URL+"/tunnel/poll"; a double slash breaks POST respond through
	// ServeMux redirects).
	return strings.TrimRight(url, "/"), key
}

// SaveConfig persists the relay configuration (`woca relay connect`).
// The URL is stored canonicalized (trailing slashes trimmed).
func SaveConfig(st *settings.Store, url, key string) error {
	url = strings.TrimRight(url, "/")
	cfg := st.Load()
	if err := settings.Apply(&cfg, settings.KeyRelayURL, url); err != nil {
		return err
	}
	if err := st.Save(cfg); err != nil {
		return err
	}
	return st.SetSecret(settings.SecretRelayKey, key)
}

// ClearConfig removes the persisted relay configuration (`woca relay
// disconnect`). Save skips empty fields by design, so the URL row goes
// through Store.Clear and the secret through SetSecret("").
func ClearConfig(st *settings.Store) error {
	if err := st.Clear("relay_url"); err != nil {
		return err
	}
	return st.SetSecret(settings.SecretRelayKey, "")
}
```

Write `internal/relayclient/service.go`:

```go
package relayclient

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/settings"
)

// Service owns the relayclient lifecycle inside `woca serve`: it reads
// the persisted (or environment) configuration, runs the tunnel against
// the server handler once available, and exposes the local status
// surface (design §3.2).
type Service struct {
	mu         sync.Mutex
	handler    http.Handler
	st         *settings.Store
	client     *Client
	cancel     context.CancelFunc
	lastStatus *Status
}

// NewService builds an idle service.
func NewService() *Service { return &Service{} }

// SetHandler receives the fully-built server handler (the servefront
// Edition.Handler seam) — the tunnel executes against exactly this
// stack, the same code path as local web traffic (design §3.2).
func (s *Service) SetHandler(h http.Handler) {
	s.mu.Lock()
	s.handler = h
	s.mu.Unlock()
}

// SetSettings receives the settings store (the servefront
// Edition.OnSettings seam) for the status surface.
func (s *Service) SetSettings(st *settings.Store) {
	s.mu.Lock()
	s.st = st
	s.mu.Unlock()
}

// Run starts the tunnel when a relay is configured (environment wins
// over persisted settings) and blocks until ctx ends or the tunnel hits
// a terminal state. Unconfigured is a no-op (local-only posture).
func (s *Service) Run(ctx context.Context, st *settings.Store, log zerolog.Logger) {
	url, key := ResolveConfig(st)
	if url == "" || key == "" {
		return
	}
	s.mu.Lock()
	for s.handler == nil {
		// SetHandler runs before Edition.Start in the serve path; this
		// wait is defensive against wiring drift.
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
		s.mu.Lock()
	}
	handler := s.handler
	runCtx, cancel := context.WithCancel(ctx)
	client := New(Config{URL: url, Key: key, Handler: handler}, log)
	s.client = client
	s.cancel = cancel
	s.mu.Unlock()

	log.Info().Str("url", url).Msg("relay: connecting bridge tunnel")
	client.Run(runCtx)

	final := client.Status()
	s.mu.Lock()
	s.lastStatus = &final
	s.client = nil
	s.cancel = nil
	s.mu.Unlock()
}

// Disconnect stops the live tunnel (`woca relay disconnect` / the local
// API). The next serve start reconnects from the persisted config.
func (s *Service) Disconnect() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Status merges the live tunnel snapshot with the persisted
// configuration.
func (s *Service) Status() Status {
	s.mu.Lock()
	st := s.st
	client := s.client
	last := s.lastStatus
	s.mu.Unlock()

	url, key := ResolveConfig(st)
	base := Status{Configured: url != "" && key != ""}
	base.KeyPrefix = key
	if len(base.KeyPrefix) > 8 {
		base.KeyPrefix = base.KeyPrefix[:8]
	}
	if client != nil {
		cur := client.Status()
		cur.Configured = base.Configured
		cur.KeyPrefix = base.KeyPrefix
		return cur
	}
	if last != nil {
		out := *last
		out.Configured = base.Configured
		out.KeyPrefix = base.KeyPrefix
		out.Connected = false
		return out
	}
	return base
}

// StatusHandler serves GET /api/relay/status (local surface — the CLI
// and the local web status page read it, design §3.2).
func (s *Service) StatusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.Status())
	})
}

// DisconnectHandler serves POST /api/relay/disconnect (stops the live
// tunnel; the persisted configuration is cleared by the CLI).
func (s *Service) DisconnectHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.Disconnect()
		w.WriteHeader(http.StatusNoContent)
	})
}
```

Modify `internal/settings/settings.go`:

1. In the secret-names block (:68-79), append after `SecretMemoryPGPassword`:

```go
	SecretRelayKey             = "relay_key"
```

2. In the `Settings` struct, after the `ModelWhitelist string` field (:117), add:

```go
	// RelayURL is the personal remote relay base URL persisted by
	// `woca relay connect` (the key lives in the secrets table under
	// SecretRelayKey). Empty = local-only.
	RelayURL string `json:"relay_url"`
```

3. In `applyPairs`, after the `model_whitelist` block (:437-439), add:

```go
	if v, ok := pairs["relay_url"]; ok {
		out.RelayURL = v
	}
```

4. In `Save`, in the second `fields = append(fields, ...)` block (:542-555), add to the list:

```go
		field{"relay_url", cfg.RelayURL},
```

Modify `internal/settings/keys.go`:

1. In the const block (:11-62), add:

```go
	KeyRelayURL              = "relay-url"
```

2. In `Apply`, add a case before `default:` (:418):

```go
	case KeyRelayURL:
		if value != "" && !strings.Contains(value, "://") {
			return fmt.Errorf("relay-url must be scheme://host, got %q", value)
		}
		cfg.RelayURL = value
```

3. In `KeyValue`, add a case:

```go
	case KeyRelayURL:
		return cfg.RelayURL
```

Modify `cmd/woca/main.go`:

1. Imports: add

```go
	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/relayclient"
```

2. Replace the serve registration (:123)

```go
	root.AddCommand(servefront.NewServeCmdWithEdition(opts, servefront.Edition{Static: webdist.DistFS()}))
```

with:

```go
	// The personal remote relay (design 2026-09-17-personal-relay): the
	// tunnel executes against the fully-built server handler — the same
	// code path as local web traffic — and starts with the serve context
	// when a relay is configured (settings or WOCA_RELAY_URL/KEY).
	relaySvc := relayclient.NewService()
	root.AddCommand(servefront.NewServeCmdWithEdition(opts, servefront.Edition{
		Static:  webdist.DistFS(),
		Handler: relaySvc.SetHandler,
		// Local status surface (design §3.2): loopback-trust local
		// posture — these routes mount with the server's extras.
		Routes: func(mux *http.ServeMux, _ authn.Provider) {
			mux.Handle("GET /api/relay/status", relaySvc.StatusHandler())
			mux.Handle("POST /api/relay/disconnect", relaySvc.DisconnectHandler())
		},
		OnSettings: relaySvc.SetSettings,
		Start: func(ctx context.Context, _ servefront.Observability, st *settings.Store, log zerolog.Logger) {
			relaySvc.Run(ctx, st, log)
		},
	}))
	root.AddCommand(newRelayCmd(relaySvc))
```

3. Append after `newConfigCmd` (which ends at :326):

```go
// newRelayCmd builds `woca relay connect|status|disconnect` (design §3.2):
// connect persists the relay coordinates (key encrypted via the settings
// secrets mechanism), serve picks them up automatically; status reads the
// local status API; disconnect stops the live tunnel and clears the
// configuration.
func newRelayCmd(svc *relayclient.Service) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "relay",
		Short: "Manage the personal remote relay (connect | status | disconnect)",
	}
	// Same --db/WOCA_DB pattern as `woca config` (parsed at Execute time).
	open := func(cmd *cobra.Command) (*settings.Store, error) {
		dbTarget, _ := cmd.Flags().GetString("db")
		if dbTarget == "" {
			dbTarget = os.Getenv("WOCA_DB")
		}
		st, err := settings.Open(dbTarget)
		if err != nil {
			return nil, err
		}
		if st == nil {
			return nil, fmt.Errorf("settings database disabled (--db none)")
		}
		return st, nil
	}
	serverFlag := func(sub *cobra.Command) {
		sub.Flags().String("server", "http://"+webgui.DefaultAddr, "local woca serve base URL for live status/disconnect")
	}

	connect := &cobra.Command{
		Use:   "connect",
		Short: "Connect this machine's woca to a woca-relay (persists --url and --key)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			url, _ := cmd.Flags().GetString("url")
			key, _ := cmd.Flags().GetString("key")
			if url == "" || key == "" {
				return fmt.Errorf("relay connect requires --url and --key")
			}
			if !strings.HasPrefix(key, relayclient.KeyPrefix) {
				return fmt.Errorf("relay keys start with %q", relayclient.KeyPrefix)
			}
			st, err := open(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			if err := relayclient.SaveConfig(st, url, key); err != nil {
				return err
			}
			fmt.Println("relay configured — `woca serve` connects automatically (restart a running serve)")
			return nil
		},
	}
	connect.Flags().String("url", os.Getenv(relayclient.EnvRelayURL), "relay base URL, e.g. https://relay.example.com (env WOCA_RELAY_URL)")
	connect.Flags().String("key", os.Getenv(relayclient.EnvRelayKey), "wcr_ rendezvous key (env WOCA_RELAY_KEY)")
	cmd.AddCommand(connect)

	status := &cobra.Command{
		Use:   "status",
		Short: "Show the relay tunnel status (live when serve is running)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			addr, _ := cmd.Flags().GetString("server")
			if live, err := fetchRelayStatus(addr); err == nil {
				printRelayStatus(live)
				return nil
			}
			// Serve is not running (or older): fall back to the persisted
			// configuration so the answer is still useful.
			st, err := open(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			svc.SetSettings(st)
			printRelayStatus(svc.Status())
			return nil
		},
	}
	serverFlag(status)
	cmd.AddCommand(status)

	disconnect := &cobra.Command{
		Use:   "disconnect",
		Short: "Stop the relay tunnel and clear the persisted configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := open(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			if err := relayclient.ClearConfig(st); err != nil {
				return err
			}
			// Best effort: stop the live tunnel too (serve may not run).
			addr, _ := cmd.Flags().GetString("server")
			req, err := http.NewRequest(http.MethodPost, addr+"/api/relay/disconnect", nil)
			if err == nil {
				if resp, err := http.DefaultClient.Do(req); err == nil {
					_ = resp.Body.Close()
				}
			}
			fmt.Println("relay disconnected")
			return nil
		},
	}
	serverFlag(disconnect)
	cmd.AddCommand(disconnect)
	return cmd
}

// fetchRelayStatus reads the live status from a running local serve.
func fetchRelayStatus(server string) (relayclient.Status, error) {
	var st relayclient.Status
	resp, err := http.Get(server + "/api/relay/status")
	if err != nil {
		return st, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("status API: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return st, err
	}
	return st, nil
}

// printRelayStatus renders the status for terminals.
func printRelayStatus(st relayclient.Status) {
	fmt.Printf("configured: %v\n", st.Configured)
	if st.KeyPrefix != "" {
		fmt.Printf("key_prefix: %s\n", st.KeyPrefix)
	}
	fmt.Printf("connected: %v\n", st.Connected)
	if st.Generation != "" {
		fmt.Printf("generation: %s\n", st.Generation)
	}
	fmt.Printf("revoked: %v\n", st.Revoked)
	if st.LastError != "" {
		fmt.Printf("last_error: %s\n", st.LastError)
	}
	fmt.Printf("served: %d\n", st.Served)
}
```

- [ ] **Step 4: Run tests to verify they pass**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
go test ./internal/relayclient/ ./internal/settings/ -count=1
go build ./cmd/woca
go vet ./internal/relayclient/ ./internal/settings/ ./cmd/woca
```

Expected: `ok` for both packages (relayclient: all Task 7 + Task 8 tests; settings: no regressions), clean build, clean vet.

CLI smoke:

```sh
go build -o build/woca ./cmd/woca
export WOCA_DB=/tmp/woca-relay-t8.db
./build/woca relay connect --url https://relay.example.com --key wcr_demo
./build/woca relay status        # serve not running → persisted config view
./build/woca relay disconnect
./build/woca relay status        # configured: false
unset WOCA_DB; rm -f /tmp/woca-relay-t8.db
```

Expected: `connect` prints `relay configured — …`; first `status` prints `configured: true`, `key_prefix: wcr_demo`, `connected: false`; `disconnect` prints `relay disconnected`; final `status` prints `configured: false`.

- [ ] **Step 5: Commit**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
git add internal/relayclient/persist.go internal/relayclient/service.go internal/relayclient/persist_test.go internal/relayclient/service_test.go internal/settings/settings.go internal/settings/keys.go cmd/woca/main.go
git commit -m "relayclient: woca relay CLI, local status API, serve lifecycle wiring"
```

---

### Task 9: Native client relay profile + Makefile/CI + e2e

**Files:**
- Create: native-client/internal/transport/relaycred.go
- Create: native-client/internal/transport/relaycred_test.go
- Create: native-client/internal/transport/relaylogin.go
- Create: native-client/internal/transport/relaylogin_test.go
- Create: native-client/internal/configstore/relaytoken_test.go
- Create: native-client/internal/bridge/relayconnect_test.go
- Modify: native-client/internal/configstore/store.go:176-214 (add `SetProfileRelayToken`/`ProfileRelayToken`/`profileRelayTokenKey` after `profileBridgeKey`)
- Modify: native-client/internal/configstore/store.go:298-324 (`DeleteProfile`: also delete the relay-token preference row)
- Modify: native-client/internal/bridge/service.go:67-78 (`nativeStore`: add the two relay-token methods)
- Modify: native-client/internal/bridge/service.go:97-126 (`Service`: `relayLogin` field + `WithRelayLogin` option)
- Modify: native-client/internal/bridge/service.go:128-153 (`Bootstrap`: stored relay token re-opens the app)
- Modify: native-client/internal/bridge/service.go (append `ConnectRelay` after `ConnectLoopback`, :197-218)
- Modify: native-client/app.go:130-144 (append `ConnectRelay` binding after `Connect`)
- Modify: native-client/main.go:93-97 (wire `WithRelayLogin` over `pinnedClient`)
- Modify: native-client/main.go:156-170 (credentials chain: relay profile branch)
- Modify: Makefile:1 (`.PHONY`: add `build-relay e2e-relay`)
- Modify: Makefile:31-33 (add `build-relay` target after `build`)
- Modify: Makefile:39 (`build-personal: web build build-relay`)
- Modify: Makefile:105-112 (extend `deps-baseline` and `check-deps` for `./cmd/woca-relay`)
- Modify: Makefile:372 (`check-editions` scan: add `./cmd/woca-relay`)
- Modify: Makefile (append after :378: `e2e-relay` target)
- Create: ci/deps-relay.txt (generated)
- Create: ci/e2e-relay.sh
- Test: native-client/internal/transport/relaycred_test.go
- Test: native-client/internal/transport/relaylogin_test.go
- Test: native-client/internal/configstore/relaytoken_test.go
- Test: native-client/internal/bridge/relayconnect_test.go

**Interfaces:**
- Consumes: `transport.CredentialProvider` interface + `transport.Credentials{Scheme, Session, Proof}` (native-client/internal/transport/handler.go:23-31 — the Director sets `Authorization: <Scheme> <Session>` at :154-156), `configstore.NormalizeBaseURL` (store.go:935), the `preferences` table + `SetProfileBridge`/`ProfileBridge`/`profileBridgeKey` pattern (store.go:176-214), `configstore.Profile` (store.go:23-28), `bridge.Service` + `ConnectionInput` + `BootstrapState`/`StateReady`/`StateAuthorizationNeeded` + `ConnectLoopback` pattern (internal/bridge/service.go:43-49,155-218), `bridge.NewService`/`ServiceOption` (service.go:109-126), `App.Connect` binding pattern (app.go:130-144), `openTestStore(t)` test helper (internal/bridge/service_test.go:381), the `pinnedClient` assembly (native-client/main.go:91-97), the production credentials closure (native-client/main.go:156-170); Makefile `SERVER_ARTIFACTS` (:271 — `woca-relay` must NOT join it), `check-editions` (:365-378), `deps-baseline`/`check-deps` (:105-112).
- Produces:
  - `type transport.StaticBearer struct{ Token string }` implementing `CredentialProvider`
  - `type transport.RelaySession struct { Token string; ExpiresAt int64 }`, `type transport.RelayLoginError struct { Status int; Code string }`
  - `func transport.RelayLogin(ctx, client *http.Client, baseURL, apiKey string) (RelaySession, error)`
  - `func (s *Store) SetProfileRelayToken(ctx, rawURL, token string) error`, `func (s *Store) ProfileRelayToken(ctx, rawURL string) string`
  - `func (s *Service) ConnectRelay(ctx, input ConnectionInput) (BootstrapState, error)`, `func WithRelayLogin(fn) ServiceOption`
  - `func (a *App) ConnectRelay(input bridge.ConnectionInput) (bridge.BootstrapState, error)` (Wails binding)
  - Make targets: `build-relay`, `e2e-relay`; baseline `ci/deps-relay.txt`; gates cover `./cmd/woca-relay`

- [ ] **Step 1: Write the failing tests**

Write `native-client/internal/transport/relaycred_test.go`:

```go
package transport

import (
	"context"
	"net/url"
	"testing"
)

func TestStaticBearer(t *testing.T) {
	p := StaticBearer{Token: "sess-abc"}
	target, err := url.Parse("https://relay.example.com")
	if err != nil {
		t.Fatal(err)
	}
	creds, err := p.Credentials(context.Background(), "GET", target)
	if err != nil {
		t.Fatal(err)
	}
	// The relay login's body token rides as a Bearer credential — the
	// native client's direct SSE path needs an explicit Bearer because
	// cookies do not reach it (design §3.3). No DPoP proof for relays.
	if creds.Scheme != "Bearer" || creds.Session != "sess-abc" || creds.Proof != "" {
		t.Fatalf("credentials: %+v", creds)
	}
	// Method and target are irrelevant to a relay session.
	creds2, err := p.Credentials(context.Background(), "POST", target)
	if err != nil || creds2 != creds {
		t.Fatalf("method-dependent credentials: %+v err=%v", creds2, err)
	}
}
```

Write `native-client/internal/configstore/relaytoken_test.go`:

```go
package configstore

import (
	"path/filepath"
	"testing"
)

func TestProfileRelayTokenRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()

	if got := s.ProfileRelayToken(ctx, "https://relay.example.com"); got != "" {
		t.Fatalf("unset token: %q", got)
	}
	if err := s.SetProfileRelayToken(ctx, "https://relay.example.com", "sess-1"); err != nil {
		t.Fatal(err)
	}
	if got := s.ProfileRelayToken(ctx, "https://relay.example.com"); got != "sess-1" {
		t.Fatalf("token: %q", got)
	}
	// Re-login rotates the token in place.
	if err := s.SetProfileRelayToken(ctx, "https://relay.example.com", "sess-2"); err != nil {
		t.Fatal(err)
	}
	if got := s.ProfileRelayToken(ctx, "https://relay.example.com"); got != "sess-2" {
		t.Fatalf("rotated token: %q", got)
	}
	// A different profile has its own row.
	if got := s.ProfileRelayToken(ctx, "https://other.example.com"); got != "" {
		t.Fatalf("cross-profile leakage: %q", got)
	}
	// Profile deletion takes the token with it (mirrors the bridge pin).
	p, err := s.SaveProfile(ctx, "https://relay.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DeleteProfile(ctx, p.ID); err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	if got := s.ProfileRelayToken(ctx, "https://relay.example.com"); got != "" {
		t.Fatalf("token survived profile deletion: %q", got)
	}
}
```

Write `native-client/internal/transport/relaylogin_test.go`:

```go
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRelayLoginSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/login" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var req struct {
			APIKey string `json:"api_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.APIKey != "wcr_test" {
			t.Errorf("login body: api_key=%q err=%v", req.APIKey, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "sess-1", "expires_at": 1700000000000})
	}))
	t.Cleanup(srv.Close)
	// A trailing slash on the profile URL must not break the join.
	sess, err := RelayLogin(context.Background(), srv.Client(), srv.URL+"/", "wcr_test")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Token != "sess-1" || sess.ExpiresAt != 1700000000000 {
		t.Fatalf("session: %+v", sess)
	}
}

func TestRelayLoginRevoked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"revoked","error":"key revoked"}`))
	}))
	t.Cleanup(srv.Close)
	_, err := RelayLogin(context.Background(), srv.Client(), srv.URL, "wcr_dead")
	var lerr *RelayLoginError
	if !errors.As(err, &lerr) {
		t.Fatalf("err %v, want *RelayLoginError", err)
	}
	if lerr.Code != "revoked" || lerr.Status != http.StatusUnauthorized {
		t.Fatalf("login error: %+v", lerr)
	}
}

func TestRelayLoginUnreachable(t *testing.T) {
	// A transport failure is a plain error, never a RelayLoginError (the
	// UI must not show "key revoked" for an offline relay).
	_, err := RelayLogin(context.Background(), http.DefaultClient, "http://127.0.0.1:1", "wcr_x")
	if err == nil {
		t.Fatal("expected an error")
	}
	var lerr *RelayLoginError
	if errors.As(err, &lerr) {
		t.Fatalf("transport failure misclassified as RelayLoginError: %v", err)
	}
}
```

Write `native-client/internal/bridge/relayconnect_test.go`:

```go
package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/projanvil/woca/native-client/internal/transport"
)

func TestConnectRelayStoresToken(t *testing.T) {
	st := openTestStore(t)
	svc := NewService(st, nil, WithRelayLogin(func(_ context.Context, baseURL, apiKey string) (transport.RelaySession, error) {
		if baseURL != "https://relay.example.com" || apiKey != "wcr_key" {
			t.Errorf("login args: %q %q", baseURL, apiKey)
		}
		return transport.RelaySession{Token: "sess-1", ExpiresAt: 1700000000000}, nil
	}))
	state, err := svc.ConnectRelay(t.Context(), ConnectionInput{ServerURL: "https://relay.example.com", APIKey: "wcr_key"})
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateReady || state.ServerURL != "https://relay.example.com" {
		t.Fatalf("state: %+v", state)
	}
	if got := st.ProfileRelayToken(t.Context(), "https://relay.example.com"); got != "sess-1" {
		t.Fatalf("stored token: %q", got)
	}
	// The stored token re-opens the app after a restart (Bootstrap).
	restarted := NewService(st, nil)
	boot, err := restarted.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if boot.State != StateReady {
		t.Fatalf("bootstrap after restart: %+v, want StateReady", boot)
	}
}

func TestConnectRelayFailureKeepsProfileUnauthorized(t *testing.T) {
	st := openTestStore(t)
	svc := NewService(st, nil, WithRelayLogin(func(context.Context, string, string) (transport.RelaySession, error) {
		return transport.RelaySession{}, &transport.RelayLoginError{Status: 401, Code: "revoked"}
	}))
	state, err := svc.ConnectRelay(t.Context(), ConnectionInput{ServerURL: "https://relay.example.com", APIKey: "wcr_dead"})
	if err == nil {
		t.Fatal("revoked key accepted")
	}
	var lerr *transport.RelayLoginError
	if !errors.As(err, &lerr) || lerr.Code != "revoked" {
		t.Fatalf("err %v, want RelayLoginError with code revoked", err)
	}
	if state.State != StateAuthorizationNeeded {
		t.Fatalf("state: %+v, want StateAuthorizationNeeded", state)
	}
	if got := st.ProfileRelayToken(t.Context(), "https://relay.example.com"); got != "" {
		t.Fatalf("token stored on failed login: %q", got)
	}
}

func TestConnectRelayRequiresKey(t *testing.T) {
	st := openTestStore(t)
	svc := NewService(st, nil)
	if _, err := svc.ConnectRelay(t.Context(), ConnectionInput{ServerURL: "https://relay.example.com"}); err == nil {
		t.Fatal("empty key accepted")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca/native-client
go test ./internal/transport/ -run 'TestStaticBearer|TestRelayLogin' -count=1
go test ./internal/configstore/ -run TestProfileRelayToken -count=1
go test ./internal/bridge/ -run TestConnectRelay -count=1
```

Expected failure: `undefined: StaticBearer`, `undefined: RelayLogin`, `undefined: RelayLoginError`, `s.SetProfileRelayToken undefined (type *Store has no field or method SetProfileRelayToken)`, `undefined: WithRelayLogin`, `svc.ConnectRelay undefined (type *Service has no field or method ConnectRelay)`.

- [ ] **Step 3: Write minimal implementation**

(a) Write `native-client/internal/transport/relaycred.go`:

```go
package transport

import (
	"context"
	"net/url"
)

// StaticBearer is the relay profile's CredentialProvider (design
// 2026-09-17-personal-relay §3.3): the relay's login response carries the
// opaque session token in its body, and the native client presents it as
// a Bearer credential on every request — direct SSE included, where the
// asset proxy's cookie channel does not reach. The transport itself
// (FlushInterval: -1, SSE-safe) needs no changes.
type StaticBearer struct {
	Token string
}

// Credentials returns the static Bearer token; method and target are
// irrelevant to a relay session.
func (s StaticBearer) Credentials(context.Context, string, *url.URL) (Credentials, error) {
	return Credentials{Scheme: "Bearer", Session: s.Token}, nil
}
```

(b) Modify `native-client/internal/configstore/store.go` — after `profileBridgeKey` (:212-214), add:

```go
// SetProfileRelayToken records the relay session token for a relay
// profile (the relay login's body token; design §3.3). Stored as an
// internal preference keyed by base URL, mirroring the bridge pin — no
// schema change, the preferences table is generic.
func (s *Store) SetProfileRelayToken(ctx context.Context, rawURL, token string) error {
	baseURL, err := NormalizeBaseURL(rawURL)
	if err != nil {
		return err
	}
	if token == "" {
		return fmt.Errorf("relay token is required")
	}
	if len(token) > 512 {
		return fmt.Errorf("relay token too long")
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO preferences (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		profileRelayTokenKey(baseURL), token, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("set profile relay token: %w", err)
	}
	return nil
}

// ProfileRelayToken returns the profile's relay session token ("" when
// unset — direct-bridge, embedded, and enterprise profiles).
func (s *Store) ProfileRelayToken(ctx context.Context, rawURL string) string {
	baseURL, err := NormalizeBaseURL(rawURL)
	if err != nil {
		return ""
	}
	var value string
	err = s.db.QueryRowContext(ctx, `SELECT value FROM preferences WHERE key = ?`, profileRelayTokenKey(baseURL)).Scan(&value)
	if err != nil {
		return ""
	}
	if len(value) > 512 {
		return ""
	}
	return value
}

func profileRelayTokenKey(baseURL string) string {
	return "connection.relay-token." + baseURL
}
```

In `DeleteProfile` (:298-324), inside the transaction after the existing bridge-pin delete, add:

```go
	if _, err := tx.ExecContext(ctx, `DELETE FROM preferences WHERE key = ?`, profileRelayTokenKey(baseURL)); err != nil {
		return false, fmt.Errorf("delete profile relay token: %w", err)
	}
```

(c) Write `native-client/internal/transport/relaylogin.go`:

```go
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// RelaySession is the relay login's body-token contract (design §3.3:
// the login response carries the opaque session token the native client
// stores via SetProfileRelayToken and later presents as a StaticBearer
// credential).
type RelaySession struct {
	Token     string
	ExpiresAt int64
}

// RelayLoginError carries the relay's enumerated failure code ("revoked"
// is the frontend contract — the connect page surfaces it distinctly).
type RelayLoginError struct {
	Status int
	Code   string
}

func (e *RelayLoginError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("relay login failed (HTTP %d): %s", e.Status, e.Code)
	}
	return fmt.Sprintf("relay login failed (HTTP %d)", e.Status)
}

// RelayLogin performs POST /api/auth/login against a woca-relay with the
// wcr_ key and returns the body token. Transport failures are plain
// errors; HTTP failures decode the relay's {"code": ...} envelope into
// *RelayLoginError.
func RelayLogin(ctx context.Context, client *http.Client, baseURL, apiKey string) (RelaySession, error) {
	if client == nil {
		client = http.DefaultClient
	}
	// Canonical join: a trailing-slash profile URL must never produce
	// "//api/auth/login" (ServeMux 301s would re-issue the POST as GET).
	endpoint := strings.TrimRight(baseURL, "/") + "/api/auth/login"
	body, err := json.Marshal(map[string]string{"api_key": apiKey})
	if err != nil {
		return RelaySession{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return RelaySession{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return RelaySession{}, fmt.Errorf("relay login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		var envelope struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&envelope)
		return RelaySession{}, &RelayLoginError{Status: resp.StatusCode, Code: envelope.Code}
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&out); err != nil {
		return RelaySession{}, fmt.Errorf("relay login: decode: %w", err)
	}
	if out.Token == "" {
		return RelaySession{}, fmt.Errorf("relay login: empty session token")
	}
	return RelaySession{Token: out.Token, ExpiresAt: out.ExpiresAt}, nil
}
```

(d) Modify `native-client/internal/bridge/service.go`:

In the `nativeStore` interface (:67-78), add:

```go
	SetProfileRelayToken(context.Context, string, string) error
	ProfileRelayToken(context.Context, string) string
```

In the `Service` struct (:97-107), add the field:

```go
	// relayLogin performs the relay's POST /api/auth/login; injectable
	// for tests and for the TOFU-pinned client (WithRelayLogin).
	relayLogin func(ctx context.Context, baseURL, apiKey string) (transport.RelaySession, error)
```

In `NewService` (:109-118), set the default and keep the option loop:

```go
func NewService(store nativeStore, activator auth.Activator, opts ...ServiceOption) *Service {
	if activator == nil {
		activator = auth.UnavailableActivator{}
	}
	s := &Service{
		store:     store,
		activator: activator,
		now:       time.Now,
		relayLogin: func(ctx context.Context, baseURL, apiKey string) (transport.RelaySession, error) {
			return transport.RelayLogin(ctx, nil, baseURL, apiKey)
		},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}
```

After `WithSessionVault` (:123-126), add:

```go
// WithRelayLogin overrides the relay login call — production wires the
// TOFU-pinned client; tests inject a stub.
func WithRelayLogin(fn func(ctx context.Context, baseURL, apiKey string) (transport.RelaySession, error)) ServiceOption {
	return func(s *Service) { s.relayLogin = fn }
}
```

In `Bootstrap` (:128-153), after the vault re-open block, add:

```go
	// A stored relay session token re-opens the app: relay sessions live
	// server-side (7d sliding + 30d absolute, same table as web
	// sessions); the local copy is a plain preference row mirroring it.
	if s.store.ProfileRelayToken(ctx, profile.BaseURL) != "" {
		s.markReady(profile.BaseURL)
		state.State = StateReady
		return state, nil
	}
```

Append after `ConnectLoopback` (:197-218):

```go
// ConnectRelay adopts a personal woca-relay profile (design §3.3): the
// wcr_ key logs in at the relay's /api/auth/login, and the response's
// body token is stored via SetProfileRelayToken — that token is what the
// transport's StaticBearer then presents on every request. The key
// itself is cleared from the input copy and never persisted.
func (s *Service) ConnectRelay(ctx context.Context, input ConnectionInput) (BootstrapState, error) {
	defer func() { input.APIKey = "" }()

	if strings.TrimSpace(input.APIKey) == "" {
		return BootstrapState{}, errors.New("relay key is required")
	}
	if s.store == nil {
		return BootstrapState{}, errors.New("connection profile store is required")
	}

	// A newly selected URL must not inherit readiness from the prior profile.
	s.clearReady()
	profile, err := s.store.SaveProfile(ctx, input.ServerURL)
	if err != nil {
		return BootstrapState{}, err
	}
	// Relay profiles are their own target — no edge bridge routing.
	if err := s.store.SetProfileBridge(ctx, input.ServerURL, ""); err != nil {
		return BootstrapState{}, err
	}
	session, err := s.relayLogin(ctx, profile.BaseURL, input.APIKey)
	if err != nil {
		return BootstrapState{State: StateAuthorizationNeeded, ServerURL: profile.BaseURL}, err
	}
	if err := s.store.SetProfileRelayToken(ctx, profile.BaseURL, session.Token); err != nil {
		return BootstrapState{State: StateAuthorizationNeeded, ServerURL: profile.BaseURL}, err
	}
	s.markReady(profile.BaseURL)
	return BootstrapState{State: StateReady, ServerURL: profile.BaseURL}, nil
}
```

(`service.go` gains an import of `github.com/projanvil/woca/native-client/internal/transport`; all bridge tests use the real `configstore.Store` via `openTestStore`, which satisfies the extended interface — no test doubles to update.)

(e) Modify `native-client/app.go` — append after `Connect` (:130-144):

```go
// ConnectRelay is the connection-page relay entry: ServerURL is the
// woca-relay base URL, APIKey the wcr_ rendezvous key. Unlike Connect,
// the key is never pushed into the bridge credential vault — the stored
// relay session token (SetProfileRelayToken) is the only credential.
func (a *App) ConnectRelay(input bridge.ConnectionInput) (bridge.BootstrapState, error) {
	if a.bridge == nil {
		return bridge.BootstrapState{}, errors.New("native bridge is required")
	}
	return a.bridge.ConnectRelay(a.context(), input)
}
```

(f) Modify `native-client/main.go`:

In the `bridge.NewService` assembly (:93-97), add the pinned relay login:

```go
	service := bridge.NewService(
		store,
		auth.NewHTTPActivator(pinnedClient, material),
		bridge.WithSessionVault(vault),
		// Relay logins ride the same TOFU-pinned client as every other
		// remote credential exchange.
		bridge.WithRelayLogin(func(ctx context.Context, baseURL, apiKey string) (transport.RelaySession, error) {
			return transport.RelayLogin(ctx, pinnedClient, baseURL, apiKey)
		}),
	)
```

And in the credentials closure (:156-170), insert the relay branch between the loopback check and the `bridgeKeys` check:

```go
	credentials, err := configureCredentials(context.Background(), service, developmentURL, func() transport.CredentialProvider {
		if profile, ok, _ := store.ActiveProfile(context.Background()); ok && isLoopbackProfileURL(profile.BaseURL) {
			return loopbackCredentials{}
		}
		// Relay profiles authenticate with the stored session token from
		// the relay login's body (design §3.3 — the direct SSE path needs
		// an explicit Bearer; cookies never reach it).
		if profile, ok, _ := store.ActiveProfile(context.Background()); ok {
			if token := store.ProfileRelayToken(context.Background(), profile.BaseURL); token != "" {
				return transport.StaticBearer{Token: token}
			}
		}
		if profile, ok, _ := store.ActiveProfile(context.Background()); ok && bridgeKeys != nil {
			base := profile.BaseURL
			return &transport.BridgeCredentials{
				BaseURL: base,
				APIKey:  func() (string, error) { return bridgeKeys.Get(base), nil },
				Client:  pinnedClient,
				DPoP:    dpop,
			}
		}
		return dpop
	})
```

(g) Modify `Makefile`:

Line 1 `.PHONY` — append ` build-relay e2e-relay` to the list.

After the `build` target (:31-33), add:

```make
# The personal remote relay (design 2026-09-17-personal-relay-design): a
# standalone public-entry binary. Deliberately NOT in SERVER_ARTIFACTS
# (Makefile:271) — it is a personal-edition artifact and the role
# TUI/frontend gates do not apply to it.
build-relay:
	go build -o build/woca-relay ./cmd/woca-relay
```

Change `build-personal` (:39) to:

```make
build-personal: web build build-relay
```

Replace `deps-baseline` and `check-deps` (:105-112) with:

```make
deps-baseline:
	@mkdir -p ci
	go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./cmd/woca | LC_ALL=C sort -u > ci/deps-personal.txt
	go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./cmd/woca-relay | LC_ALL=C sort -u > ci/deps-relay.txt

check-deps: build build-relay
	@mkdir -p build
	@go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./cmd/woca | LC_ALL=C sort -u > build/deps-personal.actual
	@diff -u ci/deps-personal.txt build/deps-personal.actual && echo "personal-edition deps match baseline"
	@go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./cmd/woca-relay | LC_ALL=C sort -u > build/deps-relay.actual
	@diff -u ci/deps-relay.txt build/deps-relay.actual && echo "relay deps match baseline"
```

In `check-editions` (:372), change the scan line from

```make
	@out=$$(go list -f '{{.ImportPath}} {{join .Imports " "}} {{join .TestImports " "}}' ./internal/... ./web/... ./cmd/woca 2>/dev/null \
```

to

```make
	@out=$$(go list -f '{{.ImportPath}} {{join .Imports " "}} {{join .TestImports " "}}' ./internal/... ./web/... ./cmd/woca ./cmd/woca-relay 2>/dev/null \
```

Append at the end of the Makefile (after :378):

```make
# Personal relay end-to-end (design §10): key create → connect → login →
# tunneled API + SSE → revoke → both sides error. Loopback, throwaway
# state; no model provider required (the run surface is exercised through
# the tunneled API and the global events SSE).
e2e-relay:
	ci/e2e-relay.sh
```

(h) Generate `ci/deps-relay.txt`:

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./cmd/woca-relay | LC_ALL=C sort -u > ci/deps-relay.txt
```

Expected contents: only stdlib-adjacent internal packages (`github.com/projanvil/woca/internal/...`, `github.com/projanvil/woca/web`), `github.com/rs/zerolog` (+ its existing transitive deps), `github.com/spf13/cobra` (+ `github.com/spf13/pflag`, `github.com/inconshreveable/mousetrap`), and `modernc.org/sqlite` (+ its existing transitive deps) — every entry already present in `ci/deps-personal.txt`, i.e. **zero new third-party dependencies**. Verify with:

```sh
comm -13 <(sort -u ci/deps-personal.txt) <(sort -u ci/deps-relay.txt)
```

Expected: empty output (no relay dependency outside the personal baseline).

(i) Write `ci/e2e-relay.sh`:

```bash
#!/usr/bin/env bash
# ci/e2e-relay.sh — personal relay end-to-end (design §10), all on
# loopback with throwaway state:
#   create key → bridge connect → relay serve + woca serve → client login
#   → tunneled API → SSE headers → revoke → client 401 + bridge revoked.
set -euo pipefail
cd "$(dirname "$0")/.."

TMP="$(mktemp -d)"
RELAY_PID=""
WOCA_PID=""
cleanup() {
  [ -n "$WOCA_PID" ] && kill "$WOCA_PID" 2>/dev/null || true
  [ -n "$RELAY_PID" ] && kill "$RELAY_PID" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

RELAY_ADDR=127.0.0.1:18443
WOCA_ADDR=127.0.0.1:13484

go build -o "$TMP/woca-relay" ./cmd/woca-relay
go build -o "$TMP/woca" ./cmd/woca

# 1. Key create — the full key is printed on stdout exactly once.
KEY="$("$TMP/woca-relay" --db "$TMP/relay.db" key create e2e | awk '/^wcr_/{print $1; exit}')"
[ -n "$KEY" ] || { echo "FAIL: no key minted"; exit 1; }

# 2. Relay serve (loopback plain HTTP is legal; --insecure not needed).
"$TMP/woca-relay" --db "$TMP/relay.db" serve --addr "$RELAY_ADDR" --log-level warn &
RELAY_PID=$!

# 3. Bridge: persist the config, then serve (the tunnel starts with it).
export WOCA_DB="$TMP/woca.db"
"$TMP/woca" relay connect --url "http://$RELAY_ADDR" --key "$KEY" >/dev/null
"$TMP/woca" serve --addr "$WOCA_ADDR" --no-browser --log-level warn &
WOCA_PID=$!

for _ in $(seq 1 50); do
  curl -fsS "http://$RELAY_ADDR/api/health" >/dev/null 2>&1 && break || sleep 0.2
done
for _ in $(seq 1 75); do
  curl -fsS "http://$WOCA_ADDR/api/health" >/dev/null 2>&1 && break || sleep 0.4
done

# 4. Client login (cookie jar) — the body token contract is asserted too.
curl -fsS -c "$TMP/cookies" -H 'Content-Type: application/json' \
  -d "{\"api_key\":\"$KEY\"}" "http://$RELAY_ADDR/api/auth/login" > "$TMP/login.json"
grep -q '"token"' "$TMP/login.json" || { echo "FAIL: login response lacks the body token (native contract)"; exit 1; }

# 5. Tunneled API: the relay forwards /api/sessions to the bridge.
OUT=""
for _ in $(seq 1 50); do
  OUT="$(curl -fsS -b "$TMP/cookies" "http://$RELAY_ADDR/api/sessions" 2>/dev/null)" && break || sleep 0.5
done
echo "${OUT:-}" | grep -q '\[' || { echo "FAIL: tunneled /api/sessions: ${OUT:-<empty>}"; exit 1; }

# 6. SSE through the tunnel: event-stream content type survives the
# flush-before-headers order, X-Accel-Buffering is set, hop-by-hop
# Connection does not leak.
HEADERS="$(curl -sS -b "$TMP/cookies" -D - -o /dev/null --max-time 5 "http://$RELAY_ADDR/api/events" 2>/dev/null || true)"
echo "$HEADERS" | grep -qi 'content-type: text/event-stream' || { echo "FAIL: SSE content type"; echo "$HEADERS"; exit 1; }
echo "$HEADERS" | grep -qi 'x-accel-buffering: no' || { echo "FAIL: X-Accel-Buffering"; echo "$HEADERS"; exit 1; }

# 7. Revoke → the sweep cascades: client requests hit the login gate,
# the bridge's status marks the key revoked.
"$TMP/woca-relay" --db "$TMP/relay.db" key revoke e2e >/dev/null
sleep 2 # revocation sweep interval (1s) plus margin
CODE="$(curl -s -o /dev/null -w '%{http_code}' -b "$TMP/cookies" "http://$RELAY_ADDR/api/sessions")"
[ "$CODE" = "401" ] || { echo "FAIL: post-revoke client status $CODE, want 401"; exit 1; }
"$TMP/woca" relay status --server "http://$WOCA_ADDR" > "$TMP/status.txt" 2>/dev/null || true
grep -qi 'revoked' "$TMP/status.txt" || { echo "FAIL: bridge status not revoked"; cat "$TMP/status.txt"; exit 1; }

# 8. The audit trail recorded the revocation.
grep -q '"event":"revocation"' "$TMP/relay.db.audit.log" || { echo "FAIL: revocation not audited"; exit 1; }

echo "e2e-relay: OK"
```

Then `chmod +x ci/e2e-relay.sh`.

- [ ] **Step 4: Run tests and gates to verify they pass**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca/native-client
go test ./... -count=1
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
go build ./... 
make check-deps
make check-editions
make e2e-relay
```

Expected, in order:

- `ok` for every native-client package (remember: native-client is a SEPARATE Go module — its tests only run under `cd native-client && go test ./...` / `make native-test`).
- Root module builds clean.
- `personal-edition deps match baseline` and `relay deps match baseline` (if `ci/deps-personal.txt` legitimately drifted because `internal/relayclient` entered `./cmd/woca`'s import graph with new *internal* packages only, run `make deps-baseline`, inspect the diff shows only `github.com/projanvil/woca/internal/...` additions, and commit both baselines).
- `edition boundaries pass dependency hygiene` (no `internal/ee` in the new code).
- `e2e-relay: OK`.

- [ ] **Step 5: Commit**

```sh
cd /Users/yuhaochen/Documents/codebase/projanvil/woca
git add native-client/internal/transport/relaycred.go native-client/internal/transport/relaycred_test.go native-client/internal/transport/relaylogin.go native-client/internal/transport/relaylogin_test.go native-client/internal/configstore/relaytoken_test.go native-client/internal/configstore/store.go native-client/internal/bridge/service.go native-client/internal/bridge/relayconnect_test.go native-client/app.go native-client/main.go Makefile ci/deps-relay.txt ci/e2e-relay.sh ci/deps-personal.txt
git commit -m "relay: native relay profile + login, build/CI gates, loopback e2e"
```

---

## Definition of Done

1. `go test ./internal/relay/ ./internal/relayclient/ ./internal/settings/ -count=1` PASS; `cd native-client && go test ./...` PASS.
2. `make check-deps` — both baselines match; `comm -13` check shows zero relay-only third-party deps.
3. `make check-editions` PASS (no `internal/ee` imports; `./cmd/woca-relay` scanned).
4. `make e2e-relay` prints `e2e-relay: OK`.
5. Revocation error code is `"revoked"` everywhere on the wire (grep the new tree for `key_revoked`: zero hits outside this document's prose and the audit event constant `AuditRevocation = "revocation"`).
6. No `golang.org/x/time/rate` anywhere; the rate limiter is the stdlib hand-rolled `RateLimiter`.
7. Full `wcr_` keys appear in no log line, no audit line, no settings plaintext, and exactly once on the `key create` stdout.
