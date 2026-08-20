# Woca Native Client Scaffold Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build and launch a Wails v2.14.0 enterprise native-client scaffold that embeds the existing Woca Vue UI, persists native connection configuration in SQLite, and proxies local `/api` HTTP/SSE traffic to a remote-BFF boundary.

**Architecture:** `native-client/` is a nested Go module, so Wails and desktop-only dependencies cannot enter the personal-edition module. It imports the root `web` package's `webdist.DistFS()`, while a small Vue bootstrap gate delays the existing application until the native bridge is ready. The first milestone implements the local persistence and transport seams plus a loopback-only development override; production OAuth/PKCE/handoff/DPoP and the remote authorization service remain the later milestones identified by the approved spec.

**Tech Stack:** Go 1.26.4, Wails v2.14.0, `modernc.org/sqlite`, Vue 3, TypeScript, Vite, Vitest, Go `httptest`

**Spec:** `docs/superpowers/specs/2026-08-20-woca-native-client-design.md`

## Global Constraints

- The personal edition remains the root `woca` binary and never asks for a server URL.
- `native-client/` is an independent nested Go module pinned to `github.com/wailsapp/wails/v2 v2.14.0`.
- The existing `web/` directory remains the only Vue/Vite source and build.
- Enterprise Web and native clients ultimately call only the remote BFF; the scaffold must not add a direct-to-service path.
- Native Vue may hold the API key only in the connection form long enough to pass one activation input across the Wails bridge; it must clear the field after submission and must never persist or log the value.
- OAuth access tokens, OAuth refresh tokens, and DPoP private keys must never enter Vue code.
- The API key must never reach SQLite, browser storage, logs, or generated files.
- Native SQLite stores only URL, non-sensitive preferences, and the future opaque native-session ciphertext.
- Production must require HTTPS; plain HTTP is allowed only for `localhost`, `127.0.0.1`, or `[::1]` development targets.
- The development authentication bypass must be compiled only with Wails' `dev` build tag and must reject non-loopback targets.
- Existing user changes in the root worktree are out of scope and must not be overwritten or included in task commits.

---

## File Structure

Files created or changed by this plan have one responsibility each:

```text
native-client/
├── go.mod                              nested desktop dependency boundary
├── go.sum                              locked native dependencies
├── wails.json                          Wails v2 project/build configuration
├── assets.go                           exposes the shared webdist filesystem
├── assets_test.go                      proves shared assets contain index.html
├── app.go                              thin Wails-bound API wrapper
├── app_test.go                         checks Wails API delegation
├── main.go                             dependency wiring and wails.Run
├── main_test.go                        verifies Wails options wiring
├── devmode_dev.go                      loopback-only dev override (`dev` tag)
├── devmode_prod.go                     no override in production
├── internal/configstore/path.go        cross-platform app-data path policy
├── internal/configstore/path_test.go   path-policy tests
├── internal/configstore/store.go       SQLite open/migrate/profile operations
├── internal/configstore/store_test.go  SQLite behavior tests
├── internal/auth/activator.go          activation interface and explicit unavailable result
├── internal/bridge/service.go          bootstrap/connection state machine
├── internal/bridge/service_test.go     state and API-key non-persistence tests
├── internal/transport/handler.go       guarded HTTP/SSE reverse transport
└── internal/transport/handler_test.go  proxy security/streaming tests

web/
├── package.json                        adds frontend test command/dependencies
├── package-lock.json                   locks frontend test dependencies
├── vitest.config.ts                    happy-dom Vue test environment
└── src/
    ├── App.vue                         native gate wrapper
    ├── WocaApp.vue                     existing application shell/startup logic
    ├── env.d.ts                         Wails bridge window typing
    ├── native/bridge.ts                typed bridge discovery
    ├── native/NativeGate.vue           bootstrap state gate
    ├── native/NativeGate.test.ts       browser/native gate behavior
    ├── native/ConnectView.vue          enterprise URL/API-key form
    ├── native/ConnectView.test.ts      form behavior and no browser persistence
    ├── i18n/locales/en/native.ts       English native-client copy
    ├── i18n/locales/zh-CN/native.ts    Chinese native-client copy
    └── i18n/locales/index.ts           registers native-client copy

Makefile                                native convenience targets
.gitignore                              ignores generated native build output
native-client/README.md                 build/run and loopback-dev instructions
```

---

### Task 1: Establish the nested native module and shared asset contract

**Files:**
- Create: `native-client/go.mod`
- Create: `native-client/assets_test.go`
- Create: `native-client/assets.go`
- Create: `native-client/wails.json`
- Generate: `native-client/go.sum`

**Interfaces:**
- Consumes: `webdist.DistFS() fs.FS` from `github.com/projanvil/woca/web`
- Produces: `func frontendAssets() fs.FS`

- [ ] **Step 1: Create the nested module manifest**

Create `native-client/go.mod` with the exact direct dependency boundary:

```go
module github.com/projanvil/woca/native-client

go 1.26.4

require (
	github.com/projanvil/woca v0.0.0
	github.com/wailsapp/wails/v2 v2.14.0
	modernc.org/sqlite v1.56.0
)

replace github.com/projanvil/woca => ..
```

- [ ] **Step 2: Write the failing shared-assets test**

Create `native-client/assets_test.go`:

```go
package main

import (
	"io/fs"
	"testing"
)

func TestFrontendAssetsContainIndex(t *testing.T) {
	assets := frontendAssets()
	if assets == nil {
		t.Fatal("frontendAssets returned nil")
	}
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		t.Fatalf("shared web index.html: %v", err)
	}
}
```

- [ ] **Step 3: Run the test and verify the intended RED state**

Run: `cd native-client && go test .`

Expected: FAIL with `undefined: frontendAssets`.

- [ ] **Step 4: Implement the minimal asset adapter**

Create `native-client/assets.go`:

```go
package main

import (
	"io/fs"

	webdist "github.com/projanvil/woca/web"
)

func frontendAssets() fs.FS {
	return webdist.DistFS()
}
```

- [ ] **Step 5: Add the Wails project configuration**

Create `native-client/wails.json`:

```json
{
  "$schema": "https://wails.io/schemas/config.v2.json",
  "name": "woca-native-client",
  "outputfilename": "woca-native-client",
  "frontend:dir": "../web",
  "frontend:install": "npm install",
  "frontend:build": "npm run build",
  "frontend:dev:build": "npm run build",
  "assetdir": "../web/dist",
  "author": {
    "name": "ProjAnvil"
  }
}
```

- [ ] **Step 6: Resolve dependencies and verify GREEN**

Run:

```bash
cd native-client
go mod tidy
go test .
```

Expected: PASS for `TestFrontendAssetsContainIndex`.

- [ ] **Step 7: Confirm module isolation**

Run:

```bash
go list -m -f '{{.Path}} {{.Version}}' github.com/wailsapp/wails/v2
cd ..
go list -m all | rg 'wailsapp/wails' || true
```

Expected: the nested module reports `v2.14.0`; the root module reports no Wails module.

- [ ] **Step 8: Commit the module foundation**

```bash
git add native-client/go.mod native-client/go.sum native-client/assets.go native-client/assets_test.go native-client/wails.json
git commit -m "feat(native): establish Wails module and shared assets"
```

---

### Task 2: Persist connection profiles in native SQLite

**Files:**
- Create: `native-client/internal/configstore/path_test.go`
- Create: `native-client/internal/configstore/path.go`
- Create: `native-client/internal/configstore/store_test.go`
- Create: `native-client/internal/configstore/store.go`

**Interfaces:**
- Produces: `func DefaultPath() (string, error)`
- Produces: `func DataDir(goos, home string, getenv func(string) string) (string, error)`
- Produces: `func Open(path string) (*Store, error)`
- Produces: `func OpenDefault() (*Store, error)`
- Produces: `func (s *Store) SaveProfile(ctx context.Context, rawURL string) (Profile, error)`
- Produces: `func (s *Store) ActiveProfile(ctx context.Context) (Profile, bool, error)`
- Produces: `func (s *Store) PutPreference(ctx context.Context, key, value string) error`
- Produces: `func (s *Store) Preference(ctx context.Context, key string) (string, bool, error)`
- Produces: `func (s *Store) Close() error`

- [ ] **Step 1: Write failing platform-path tests**

Create `native-client/internal/configstore/path_test.go` with table cases asserting:

```go
func TestDataDir(t *testing.T) {
	tests := []struct {
		name string
		goos string
		home string
		env map[string]string
		want string
	}{
		{"darwin", "darwin", "/Users/alice", nil, "/Users/alice/Library/Application Support/woca-native-client"},
		{"windows", "windows", `C:\\Users\\alice`, map[string]string{"APPDATA": `C:\\Users\\alice\\AppData\\Roaming`}, `C:\\Users\\alice\\AppData\\Roaming\\woca-native-client`},
		{"linux xdg", "linux", "/home/alice", map[string]string{"XDG_DATA_HOME": "/data/alice"}, "/data/alice/woca-native-client"},
		{"linux fallback", "linux", "/home/alice", nil, "/home/alice/.local/share/woca-native-client"},
	}
	// Use filepath.Clean on want/got before comparison so the test runs on macOS.
}
```

Also assert that a relative `XDG_DATA_HOME`, missing Windows `APPDATA`, or
missing home directory returns an error rather than a relative path.

- [ ] **Step 2: Run path tests and verify RED**

Run: `cd native-client && go test ./internal/configstore -run TestDataDir -v`

Expected: FAIL because `DataDir` does not exist.

- [ ] **Step 3: Implement deterministic path selection**

Create `path.go` using `runtime.GOOS`, `os.UserHomeDir`, and `os.Getenv` only in
`DefaultPath`; keep all platform branching inside the injected `DataDir`
function. Return `filepath.Join(dataDir, "client.db")` from `DefaultPath`.

- [ ] **Step 4: Verify path tests pass**

Run: `cd native-client && go test ./internal/configstore -run TestDataDir -v`

Expected: PASS.

- [ ] **Step 5: Write failing SQLite behavior tests**

Create `store_test.go` with separate tests for these behaviors:

```go
func TestStorePersistsActiveProfileAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	s, err := Open(path)
	if err != nil { t.Fatal(err) }
	got, err := s.SaveProfile(t.Context(), "https://Enterprise.Example.com/")
	if err != nil { t.Fatal(err) }
	if got.BaseURL != "https://enterprise.example.com" { t.Fatalf("BaseURL = %q", got.BaseURL) }
	if err := s.Close(); err != nil { t.Fatal(err) }

	s, err = Open(path)
	if err != nil { t.Fatal(err) }
	defer s.Close()
	active, ok, err := s.ActiveProfile(t.Context())
	if err != nil || !ok { t.Fatalf("ActiveProfile = %#v, %v, %v", active, ok, err) }
	if active.ID != got.ID || active.BaseURL != got.BaseURL { t.Fatalf("active = %#v, want %#v", active, got) }
}

```

Add four more tests with these exact fixtures and assertions:

| Test | Fixture | Assertion |
|---|---|---|
| `TestStorePersistsPreference` | Put `theme=dark`, close, reopen | `Preference("theme")` returns `dark, true, nil` |
| `TestStoreRejectsURLWithCredentialsQueryOrFragment` | `https://u:p@example.com`, `https://example.com?q=1`, `https://example.com/#x`, `https://example.com/app` | Every `SaveProfile` returns an error and no active profile exists |
| `TestStoreAllowsHTTPOnlyForLoopback` | `http://localhost:8080`, `http://127.0.0.1:8080`, `http://[::1]:8080`, `http://example.com` | First three normalize successfully; remote HTTP returns an error |
| `TestStoreSchemaHasNoAPIKeyOrOAuthTokenColumns` | Query `sqlite_master`, then `PRAGMA table_info` for every application table | No normalized column name contains `apikey`, `api_key`, `access_token`, or `refresh_token` |

The schema-column test must fail if a normalized column name contains
`apikey`, `api_key`, `access_token`, or `refresh_token`.

- [ ] **Step 6: Run the store tests and verify RED**

Run: `cd native-client && go test ./internal/configstore -v`

Expected: FAIL because `Open`, `Store`, and profile operations do not exist.

- [ ] **Step 7: Implement the minimal SQLite store and migration**

Create `store.go` with:

```go
type Profile struct {
	ID        int64  `json:"id"`
	BaseURL   string `json:"base_url"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type Store struct { db *sql.DB }
```

Open SQLite using the `modernc.org/sqlite` driver, create the parent directory
with `0700`, enable foreign keys and a busy timeout, and apply one transaction
containing these tables:

```sql
CREATE TABLE IF NOT EXISTS schema_migrations (
  version INTEGER PRIMARY KEY,
  applied_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS connection_profiles (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  base_url TEXT NOT NULL UNIQUE,
  active INTEGER NOT NULL DEFAULT 0 CHECK (active IN (0, 1)),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS one_active_profile
  ON connection_profiles(active) WHERE active = 1;
CREATE TABLE IF NOT EXISTS preferences (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS native_sessions (
  profile_id INTEGER PRIMARY KEY REFERENCES connection_profiles(id) ON DELETE CASCADE,
  device_id TEXT NOT NULL,
  session_ciphertext BLOB NOT NULL,
  expires_at INTEGER NOT NULL,
  last_validated_at INTEGER NOT NULL
);
```

Normalize the origin with `net/url`: lowercase scheme/host, strip a trailing
slash, require an empty username/query/fragment and an empty path or `/`, and
apply the HTTPS/loopback rule from the global constraints. `SaveProfile` must
atomically clear the previous active row and upsert the selected origin.

- [ ] **Step 8: Verify configstore GREEN**

Run: `cd native-client && go test ./internal/configstore -v`

Expected: all configstore tests PASS.

- [ ] **Step 9: Commit SQLite persistence**

```bash
git add native-client/internal/configstore
git commit -m "feat(native): persist enterprise connection profile"
```

---

### Task 3: Implement a guarded remote-BFF HTTP/SSE transport

**Files:**
- Create: `native-client/internal/transport/handler_test.go`
- Create: `native-client/internal/transport/handler.go`

**Interfaces:**
- Consumes: `configstore.Profile`
- Produces: `type TargetProvider interface { ActiveProfile(context.Context) (configstore.Profile, bool, error) }`
- Produces: `type CredentialProvider interface { Credentials(context.Context, string, *url.URL) (Credentials, error) }`
- Produces: `type Credentials struct { Scheme, Session, Proof string }`
- Produces: `func New(targets TargetProvider, credentials CredentialProvider, client *http.Client) http.Handler`

- [ ] **Step 1: Write failing request-preservation and security tests**

Create `handler_test.go` using real `httptest.Server` instances. Test these
behaviors independently:

| Test | Upstream/request arrangement | Required assertion |
|---|---|---|
| `TestHandlerForwardsMethodPathQueryAndBody` | POST `/api/runs?q=1` with body `{"prompt":"hello"}` | Upstream observes the exact method, path, query, and body; client receives upstream status/body |
| `TestHandlerInjectsTrustedSessionAndDPoP` | Credential fake returns `DPoP`, `session-1`, `proof-1` | Upstream receives `Authorization: DPoP session-1` and `DPoP: proof-1` |
| `TestHandlerStripsCallerAuthCookieAndForwardingHeaders` | Caller supplies attacker values for every stripped header | Upstream sees none of the attacker values |
| `TestHandlerRejectsNonAPIPaths` | Request `/settings` | Response is JSON `404` and upstream hit count remains zero |
| `TestHandlerReturnsServiceUnavailableWithoutActiveProfile` | Target fake returns `ok=false` | Response is JSON `503` and contains no credential data |
| `TestHandlerRefusesCrossOriginRedirect` | Upstream returns `302 Location: https://evil.example/x` | Response is sanitized JSON `502`; no request reaches the redirect target |
| `TestHandlerPropagatesCancellation` | Upstream blocks on `r.Context().Done()` | Cancelling the client request closes the upstream context before the test deadline |

Use small real fakes:

```go
type staticTarget struct { profile configstore.Profile; ok bool; err error }
type staticCredentials struct { value Credentials; err error }
```

- [ ] **Step 2: Run transport tests and verify RED**

Run: `cd native-client && go test ./internal/transport -v`

Expected: FAIL because `New`, interfaces, and credentials do not exist.

- [ ] **Step 3: Implement the minimal guarded reverse transport**

Implement `handler.go` with `httputil.ReverseProxy` and a per-request director:

- Resolve the active normalized profile.
- Accept only paths beginning `/api/` or exactly `/api`.
- Clone the request, set the target scheme/host, and preserve escaped path/query.
- Remove `Authorization`, `DPoP`, `Cookie`, `Forwarded`, `X-Forwarded-*`,
  `Connection`, `Proxy-Connection`, `Keep-Alive`, `TE`, `Trailer`,
  `Transfer-Encoding`, and `Upgrade` before trusted injection.
- Set `Authorization` to `<Scheme> <Session>` and `DPoP` to `<Proof>` only from
  `CredentialProvider`.
- Use `FlushInterval: -1` to flush SSE and other streaming responses
  immediately.
- Use an `http.Client` transport with a bounded dial/TLS/header timeout but no
  whole-response timeout.
- Disable automatic redirects and map cross-origin redirect responses to a
  sanitized `502` JSON error.
- Return JSON errors with `Content-Type: application/json` and no secrets.

- [ ] **Step 4: Verify HTTP/security tests GREEN**

Run: `cd native-client && go test ./internal/transport -v`

Expected: all current transport tests PASS.

- [ ] **Step 5: Add and verify a real incremental SSE test**

Append:

```go
func TestHandlerFlushesSSEIncrementally(t *testing.T) {
	// Upstream writes "event: first\ndata: {}\n\n", flushes, waits on a channel,
	// then writes the terminal event. The client must read the first frame before
	// the test releases the channel; otherwise the handler buffered the stream.
}
```

Temporarily change `FlushInterval` to `0`, run the test, and verify it times out
or fails to observe the first frame. Restore `-1`, rerun, and verify PASS.

- [ ] **Step 6: Run the race detector for the transport**

Run: `cd native-client && go test -race ./internal/transport -v`

Expected: PASS with no races.

- [ ] **Step 7: Commit the remote transport**

```bash
git add native-client/internal/transport
git commit -m "feat(native): proxy API traffic to the remote BFF"
```

---

### Task 4: Add the native bootstrap state machine and thin Wails API

**Files:**
- Create: `native-client/internal/auth/activator.go`
- Create: `native-client/internal/bridge/service_test.go`
- Create: `native-client/internal/bridge/service.go`
- Create: `native-client/app_test.go`
- Create: `native-client/app.go`

**Interfaces:**
- Consumes: configstore profile operations from Task 2
- Produces: `auth.Activator.Activate(context.Context, auth.ActivationRequest) (auth.ActivationResult, error)`
- Produces: `bridge.State` with values `unconfigured`, `authorization_required`, and `ready`
- Produces: `bridge.Service.Bootstrap(context.Context) (BootstrapState, error)`
- Produces: `bridge.Service.Connect(context.Context, ConnectionInput) (BootstrapState, error)`
- Produces: `bridge.Service.Logout(context.Context) (BootstrapState, error)`
- Produces Wails bindings: `App.Bootstrap()`, `App.Connect(ConnectionInput)`, `App.Logout()`

- [ ] **Step 1: Define the activation seam without a production bypass**

Create `internal/auth/activator.go`:

```go
package auth

import (
	"context"
	"errors"
)

var ErrServerContractUnavailable = errors.New("native activation endpoint is not available")

type ActivationRequest struct {
	BaseURL string
	APIKey  string
}

type ActivationResult struct {
	Ready bool
}

type Activator interface {
	Activate(context.Context, ActivationRequest) (ActivationResult, error)
}

type UnavailableActivator struct{}

func (UnavailableActivator) Activate(context.Context, ActivationRequest) (ActivationResult, error) {
	return ActivationResult{}, ErrServerContractUnavailable
}
```

The explicit unavailable result ensures the production scaffold cannot silently
reinterpret the API key as a Bearer token.

- [ ] **Step 2: Write failing bootstrap state tests**

Create `service_test.go` with a real temporary SQLite store and a recording fake
activator. Cover:

| Test | Setup | Required assertion |
|---|---|---|
| `TestBootstrapIsUnconfiguredWithoutProfile` | Empty temporary store | State is `unconfigured` and URL is empty |
| `TestConnectPersistsURLButNeverAPIKey` | Connect with `https://enterprise.example.com` and unique key `woca_test_never_persist_7f31` | Activator records the key; active profile contains only the URL; no SQLite/WAL/SHM bytes contain the key |
| `TestConnectReadyWhenActivatorReturnsReady` | Recording activator returns `Ready: true` | Returned state is `ready` with normalized URL |
| `TestConnectRetainsURLWhenActivationUnavailable` | Activator returns `ErrServerContractUnavailable` | Connect returns that error and the active profile still contains the normalized URL |
| `TestBootstrapRequiresAuthorizationForStoredProfileWithoutSession` | Pre-save an active profile and construct a fresh service | State is `authorization_required` with the stored URL |

The API-key test closes SQLite and reads `client.db`, `client.db-wal`, and
`client.db-shm` when present; the unique test key must not occur in any file.

- [ ] **Step 3: Run bridge tests and verify RED**

Run: `cd native-client && go test ./internal/bridge -v`

Expected: FAIL because `Service`, `Bootstrap`, and `Connect` do not exist.

- [ ] **Step 4: Implement the bridge state machine**

Create `service.go` with:

```go
type State string

const (
	StateUnconfigured         State = "unconfigured"
	StateAuthorizationNeeded State = "authorization_required"
	StateReady                State = "ready"
)

type BootstrapState struct {
	State     State  `json:"state"`
	ServerURL string `json:"server_url,omitempty"`
	Message   string `json:"message,omitempty"`
}

type ConnectionInput struct {
	ServerURL string `json:"server_url"`
	APIKey    string `json:"api_key"`
}
```

`Connect` must reject a blank API key before persistence, save only the
normalized URL, pass the raw key directly to `Activator`, overwrite the input
field with an empty string before return, and never log the request. A ready
activation is held in process state for this scaffold; the production opaque
session persistence arrives with the later auth milestone.

`Logout` clears only the in-process ready state in this scaffold and returns
`authorization_required` with the active profile URL. It must not delete the
profile or preferences. The later auth milestone extends the same method to
delete the encrypted opaque session and revoke it at the BFF.

- [ ] **Step 5: Verify bridge tests GREEN**

Run: `cd native-client && go test ./internal/bridge -v`

Expected: PASS.

- [ ] **Step 6: Write failing thin-wrapper tests for the Wails-bound App**

Create `app_test.go` using a fake service interface and assert `App.Bootstrap`,
`App.Connect`, and `App.Logout` delegate with the context captured by
`App.startup(ctx)`.

- [ ] **Step 7: Implement the Wails App wrapper**

Create `app.go` with a private interface:

```go
type bridgeAPI interface {
	Bootstrap(context.Context) (bridge.BootstrapState, error)
	Connect(context.Context, bridge.ConnectionInput) (bridge.BootstrapState, error)
	Logout(context.Context) (bridge.BootstrapState, error)
}

type App struct {
	ctx context.Context
	bridge bridgeAPI
}
```

Expose JSON-compatible methods with no database, token, or HTTP implementation
inside this wrapper.

- [ ] **Step 8: Run native tests and commit**

Run: `cd native-client && go test ./...`

Expected: PASS.

```bash
git add native-client/internal/auth native-client/internal/bridge native-client/app.go native-client/app_test.go
git commit -m "feat(native): add enterprise bootstrap bridge"
```

---

### Task 5: Gate the existing Vue application behind native readiness

**Files:**
- Modify: `web/package.json`
- Modify: `web/package-lock.json`
- Create: `web/vitest.config.ts`
- Modify: `web/src/env.d.ts`
- Create: `web/src/native/bridge.ts`
- Create: `web/src/native/NativeGate.test.ts`
- Create: `web/src/native/NativeGate.vue`
- Create: `web/src/native/ConnectView.test.ts`
- Create: `web/src/native/ConnectView.vue`
- Create: `web/src/WocaApp.vue`
- Modify: `web/src/App.vue`
- Create: `web/src/i18n/locales/en/native.ts`
- Create: `web/src/i18n/locales/zh-CN/native.ts`
- Modify: `web/src/i18n/locales/index.ts`

**Interfaces:**
- Consumes: Wails-bound `window.go.main.App.Bootstrap/Connect/Logout`
- Produces: `getNativeBridge(): NativeBridge | null`
- Produces: `NativeGate` default slot that mounts only in ready/browser mode
- Preserves: existing relative `/api` client and existing `WocaApp` behavior

- [ ] **Step 1: Install the frontend test harness**

Run:

```bash
cd web
npm install --save-dev vitest @vue/test-utils happy-dom
```

Add scripts:

```json
"test": "vitest run",
"test:watch": "vitest"
```

Create `vitest.config.ts`:

```ts
import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  test: {
    environment: 'happy-dom',
    restoreMocks: true,
  },
})
```

- [ ] **Step 2: Add the typed bridge declaration**

Extend `env.d.ts` with `BootstrapState`, `ConnectionInput`, and a global
`Window.go?.main?.App` shape whose methods return promises. Create
`native/bridge.ts` so `getNativeBridge()` returns that object or `null` without
throwing in a normal browser.

- [ ] **Step 3: Write failing NativeGate tests**

Create `NativeGate.test.ts` with these independent cases:

| Test name | Bridge result | Required DOM assertion |
|---|---|---|
| `renders the app immediately in a normal browser` | `window.go` absent | Default slot marker is visible after mount |
| `does not mount the app before native bootstrap resolves` | Deferred `Bootstrap` promise | Slot marker is absent until resolution |
| `shows the connection view for an unconfigured native client` | `{state:'unconfigured'}` | Stubbed connection-view marker is visible; slot marker is absent |
| `mounts the app after native state becomes ready` | `{state:'ready'}` | Slot marker is visible; connection marker is absent |
| `keeps the server URL when authorization is required` | `{state:'authorization_required',server_url:'https://enterprise.example.com'}` | Connection stub receives that exact `serverUrl` prop |

Stub `ConnectView` in gate-only tests so they test the state boundary rather
than form internals.

- [ ] **Step 4: Run the gate tests and verify RED**

Run: `cd web && npm test -- src/native/NativeGate.test.ts`

Expected: FAIL because `NativeGate.vue` does not exist.

- [ ] **Step 5: Implement NativeGate minimally**

The component must:

- Render its slot immediately when `getNativeBridge()` is `null`.
- Show a neutral loading shell while `Bootstrap()` is pending.
- Render `ConnectView` for `unconfigured` and `authorization_required`.
- Render its slot only for `ready`.
- Convert bridge rejection into a visible retryable error without mounting the
  main application.

- [ ] **Step 6: Verify NativeGate GREEN**

Run: `cd web && npm test -- src/native/NativeGate.test.ts`

Expected: all gate tests PASS.

- [ ] **Step 7: Write failing ConnectView tests**

Create `ConnectView.test.ts` and test:

| Test name | Action | Required assertion |
|---|---|---|
| `requires both an enterprise URL and API key` | Submit with each field blank in turn | Connect callback remains uncalled and validation copy is visible |
| `submits URL and API key only to the provided callback` | Enter `https://enterprise.example.com` and `woca_test_key`, submit | Callback receives exactly `{server_url,api_key}` once |
| `never writes the API key to localStorage or sessionStorage` | Spy on both storage `setItem` methods and submit | Neither spy receives `woca_test_key` or any call from the component |
| `clears the API key field after a failed connection` | Callback rejects `new Error('activation failed')` | URL input retains its value, password input becomes empty, sanitized error is visible |

- [ ] **Step 8: Implement ConnectView and localized copy**

Use password input autocomplete `off`, disable submit while pending, and render
sanitized bridge errors. Add exact English/Chinese keys for title, URL label,
API-key label, connect action, retry, invalid input, and authorization-required
copy. Register the `native` locale namespace in `locales/index.ts`.

- [ ] **Step 9: Split the current application shell and add the gate**

Move the current `App.vue` script/template unchanged into `WocaApp.vue`. Replace
`App.vue` with:

```vue
<script setup lang="ts">
import NativeGate from './native/NativeGate.vue'
import WocaApp from './WocaApp.vue'
</script>

<template>
  <NativeGate>
    <WocaApp />
  </NativeGate>
</template>
```

This guarantees the existing Pinia store initialization in `WocaApp` does not
run until native readiness, because Vue does not instantiate the slot subtree
while the gate withholds it.

- [ ] **Step 10: Run frontend verification**

Run:

```bash
cd web
npm test
npm run build
```

Expected: all Vitest tests PASS; Vue type-check and Vite build exit 0.

- [ ] **Step 11: Commit the shared UI gate**

```bash
git add web/package.json web/package-lock.json web/vitest.config.ts web/src
git commit -m "feat(web): add native enterprise connection gate"
```

---

### Task 6: Wire Wails, SQLite, bridge, transport, and loopback development

**Files:**
- Create: `native-client/main_test.go`
- Create: `native-client/main.go`
- Create: `native-client/devmode_dev.go`
- Create: `native-client/devmode_prod.go`
- Modify: `native-client/internal/bridge/service.go`
- Modify: `native-client/internal/bridge/service_test.go`

**Interfaces:**
- Consumes: `frontendAssets`, `configstore.OpenDefault`, `bridge.Service`, and `transport.New`
- Produces: `func buildOptions(app *App, assets fs.FS, handler http.Handler) *options.App`
- Produces: `func developmentServerURL() string`, available only under the `dev` build tag

- [ ] **Step 1: Write the failing Wails options test**

Create `main_test.go`:

```go
func TestBuildOptionsUsesSharedAssetsTransportAndBridge(t *testing.T) {
	assets := fstest.MapFS{"index.html": {Data: []byte("ok")}}
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	app := &App{}
	opts := buildOptions(app, assets, handler)
	if opts.Title != "Woca Enterprise" { t.Fatalf("Title = %q", opts.Title) }
	if opts.AssetServer.Assets == nil || opts.AssetServer.Handler == nil { t.Fatal("asset server not wired") }
	if len(opts.Bind) != 1 || opts.Bind[0] != app { t.Fatalf("Bind = %#v", opts.Bind) }
}
```

- [ ] **Step 2: Run the test and verify RED**

Run: `cd native-client && go test . -run TestBuildOptionsUsesSharedAssetsTransportAndBridge -v`

Expected: FAIL because `buildOptions` does not exist.

- [ ] **Step 3: Add build-tagged development configuration**

Create `devmode_dev.go`:

```go
//go:build dev

package main

import "os"

func developmentServerURL() string { return os.Getenv("WOCA_NATIVE_DEV_BFF_URL") }
```

Create `devmode_prod.go`:

```go
//go:build !dev

package main

func developmentServerURL() string { return "" }
```

Add a tested bridge method that applies this override only when the URL passes
the existing loopback HTTP policy. A non-loopback override returns a startup
error. It saves the loopback profile, marks in-process readiness, and uses an
empty development credential provider; it never accepts or synthesizes an API
key.

- [ ] **Step 4: Implement Wails dependency wiring**

Create `main.go` with:

- `buildOptions` setting title `Woca Enterprise`, 1280x800 initial size, 1024x640
  minimum size, a dark neutral background, shared assets, transport handler,
  `OnStartup: app.startup`, `OnShutdown` cleanup, and `Bind: []interface{}{app}`.
- `run()` opening the default SQLite store, applying the optional loopback dev
  override, constructing `bridge.Service`, `App`, and `transport.Handler`, then
  calling `wails.Run`.
- `main()` logging only the returned error and exiting nonzero; never log bridge
  inputs, sessions, or request headers.

The production credential provider returns a structured unauthorized error
until the later handoff/DPoP milestone supplies an opaque session. The dev
credential provider returns empty credentials only for the already validated
loopback profile.

- [ ] **Step 5: Verify native unit tests and production compilation**

Run:

```bash
cd native-client
go test ./...
go build ./...
go test -tags dev ./...
```

Expected: all tests PASS and both production/dev variants compile.

- [ ] **Step 6: Install and verify the requested Wails CLI**

Run:

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@v2.14.0
wails version
```

Expected: `v2.14.0`.

- [ ] **Step 7: Build the desktop application**

Run:

```bash
cd native-client
wails build -clean
```

Expected: exit 0 and a macOS application/binary under `native-client/build/bin`.
If Wails creates platform resource templates, keep only required source assets
under `native-client/build/` and ignore generated binaries.

- [ ] **Step 8: Commit Wails wiring**

```bash
git add native-client/main.go native-client/main_test.go native-client/app.go native-client/devmode_dev.go native-client/devmode_prod.go native-client/internal/bridge native-client/build
git commit -m "feat(native): wire the Wails enterprise application"
```

---

### Task 7: Add developer commands, document the boundary, and verify runtime

**Files:**
- Modify: `Makefile`
- Modify: `.gitignore`
- Create: `native-client/README.md`

**Interfaces:**
- Produces commands: `make native-test`, `make native-build`, `make native-dev`
- Preserves commands: `make`, `make build`, `make build-personal`, `make serve`

- [ ] **Step 1: Add a failing Makefile contract check**

Run before editing:

```bash
make -n native-test
make -n native-build
make -n native-dev
```

Expected: FAIL with `No rule to make target`.

- [ ] **Step 2: Add native convenience targets without changing defaults**

Append phony targets:

```make
native-test:
	cd native-client && go test ./...

native-build: web
	cd native-client && wails build -clean

native-dev:
	cd native-client && wails dev
```

Add the three names to `.PHONY`. Do not add any native target as a dependency of
`all`, `build`, `build-personal`, or `serve`.

- [ ] **Step 3: Ignore only generated native output**

Add `native-client/build/bin/` to `.gitignore`. Do not ignore the entire
`native-client/build/` directory because Wails source icons/platform metadata
belong in version control.

- [ ] **Step 4: Document exact build and development flows**

Create `native-client/README.md` covering:

- Enterprise-only product boundary.
- Wails CLI v2.14.0 installation.
- `make native-test`, `make native-build`, and `make native-dev`.
- Production first-launch URL/API-key screen.
- Loopback-only visual development command:

```bash
./build/woca serve --addr 127.0.0.1:8899 --no-browser
WOCA_NATIVE_DEV_BFF_URL=http://127.0.0.1:8899 make native-dev
```

- The explicit statement that the local loopback service is a UI/transport
  development fixture, not the enterprise production authentication path.
- The later remote BFF/PKCE/handoff/DPoP milestone boundary and a link to the
  approved design spec.

- [ ] **Step 5: Verify Makefile contracts GREEN**

Run:

```bash
make -n native-test
make -n native-build
make -n native-dev
```

Expected: each prints the intended nested-module command.

- [ ] **Step 6: Run the full automated verification suite**

Run from the repository root:

```bash
cd web && npm test && npm run build
cd ..
go test ./...
make check-deps
cd native-client
go test -race ./...
wails build -clean
```

Expected: every command exits 0; root dependency check reports
`personal-edition deps match baseline`; Wails produces the native artifact.

- [ ] **Step 7: Launch and visually verify the Woca Web UI in Wails**

In terminal 1:

```bash
./build/woca serve --addr 127.0.0.1:8899 --no-browser
```

In terminal 2:

```bash
WOCA_NATIVE_DEV_BFF_URL=http://127.0.0.1:8899 make native-dev
```

Verify all of the following in the native window:

- The Woca sidebar, chat panel, and workspace panel render.
- Session list HTTP requests complete through the native handler.
- Starting a run receives SSE frames incrementally through the native handler.
- Closing and restarting Wails preserves the loopback URL in `client.db`.
- A production-tag build with no profile shows the enterprise URL/API-key gate.

Capture one screenshot of the native window for verification evidence; do not
commit the screenshot unless requested.

- [ ] **Step 8: Audit the first-milestone requirements**

Re-read all ten acceptance criteria in the design spec section 13 and identify
the exact file, test output, build artifact, or runtime observation proving each
one. Any missing or indirect evidence is unfinished work, not a pass.

- [ ] **Step 9: Commit developer workflow and documentation**

```bash
git add Makefile .gitignore native-client/README.md
git commit -m "docs(native): add desktop build and development workflow"
```

- [ ] **Step 10: Run final clean-state verification**

Run the complete Step 6 command sequence again after the final commit. Record
the fresh exit status and relevant test/build counts for the handoff report.
