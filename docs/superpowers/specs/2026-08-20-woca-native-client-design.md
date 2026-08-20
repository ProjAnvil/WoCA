# Woca Enterprise Native Client Design

**Date:** 2026-08-20

**Status:** Approved; scaffold implemented (production OAuth/DPoP remains a later milestone)

**Target:** Wails v2.14.0

## 1. Objective

Add a new `native-client` module to the Woca monorepo. The module packages the
existing Vue/Vite Web UI in a Wails desktop application and connects only to a
remotely deployed Woca Enterprise edge/BFF.

This work does not turn the personal edition into a desktop application. The
personal edition remains the existing `woca` binary and its same-origin browser
UI. It never asks for a server URL.

The first implementation milestone must provide a complete Wails scaffold that
can be built and launched on the development Mac, displays the existing Woca
Web UI, persists native connection settings in SQLite, and contains a tested
HTTP/SSE transport boundary for a remote BFF. The production OAuth/DPoP server
and its isolated authorization PostgreSQL database are subsequent server-side
milestones, but their contracts and trust boundaries are fixed by this design.

## 2. Product Boundaries

### 2.1 Personal edition

- Keeps the existing root Go module and `cmd/woca` executable.
- Serves the existing Web UI and `/api` from the same local Woca process.
- Stores no connection URL.
- Does not import Wails or native-client packages.

### 2.2 Enterprise Web

- Uses the same Vue/Vite source under `web/`.
- Its launcher remembers the selected Enterprise Woca URL and non-sensitive UI
  preferences in browser storage.
- It navigates the top-level browser window to the selected enterprise origin,
  which serves the Web UI and remote BFF on the same origin.
- The browser holds only a `Secure`, `HttpOnly`, `SameSite` session cookie.
- API keys and OAuth access/refresh tokens are never persisted in Web Storage.
  Enterprise Web collects the API key only on a BFF-hosted activation page,
  outside the main Vue application, and discards it after activation.

### 2.3 Enterprise native client

- Is a separate distributable Wails desktop application under
  `native-client/`.
- On first launch, its Vue bootstrap form asks for the Enterprise Woca URL and
  a server-issued API key. The form holds the key only until it passes the
  activation input to the Wails bridge, then clears it.
- Uses the API key only to bootstrap a tenant/device authorization transaction;
  it does not persist the API key.
- Opens the system browser for Authorization Code + PKCE user authorization.
- Connects exclusively to the same remote BFF used by Enterprise Web.
- Stores URL, local preferences, and an opaque native session reference in its
  application SQLite database.
- Never stores OAuth access or refresh tokens; those remain behind the remote
  BFF.

## 3. Chosen Repository Approach

The native client is an independent nested Go module that imports the root
module's `web` package and uses `webdist.DistFS()` as its Wails assets. Its
`wails.json` points `frontend:dir` at `../web`, so the existing project remains
the only frontend build.

This approach was selected over:

1. A `cmd/woca-native` entry in the root module, which would mix Wails and
   platform dependencies into the personal-edition module boundary.
2. A copied frontend under `native-client/`, which would create two UIs that
   drift over time.

Wails v2.14.0 accepts `fs.FS` for `assetserver.Options.Assets`, so the existing
`webdist.DistFS()` is directly compatible. No asset copy step or symlink is
required.

## 4. System Architecture

```text
Personal edition
  Browser Vue ── same-origin /api ──> local woca server

Enterprise Web
  Browser Vue ── HttpOnly cookie ──> remote BFF ──> Woca services

Enterprise native
  Embedded Vue ── local /api ──> Wails native bridge
                                      │ opaque session + DPoP proof
                                      v
                                  remote BFF ──> Woca services
```

The Wails Go process is a **native bridge**, not an authorization service and
not the enterprise BFF. It owns local persistence, the system-browser/loopback
handoff, device-key access, request proof generation, and transport. Tenant
authorization and downstream routing remain in the remote BFF.

The public enterprise origin has one external surface:

```text
https://enterprise.example.com
├── /app/*           Enterprise Web UI
├── /auth/*          activation, authorization, handoff, logout
├── /api/*           cookie or native-session authenticated API
└── /.well-known/*   OAuth/OIDC and protected-resource metadata
```

Downstream Woca services and both PostgreSQL data sources are not client-
reachable.

## 5. Authentication and Session Design

### 5.1 API-key bootstrap

An enterprise administrator configures a tenant and generates an API key in
the authorization service. Self-registration is not supported.

The API key establishes tenant/device enrollment eligibility; it is not a
browser client secret, a local encryption key, or a user authorization token.
Clients transmit it only over HTTPS during activation and discard it after a
successful activation transaction. The authorization database stores only the
server-side key record/hash and its issuance, expiry, and revocation metadata.

### 5.2 Native authorization flow

1. The native bridge validates the configured HTTPS enterprise origin. Plain
   HTTP is allowed only for loopback development origins.
2. It generates a per-installation asymmetric device key pair.
3. It opens a loopback listener on `127.0.0.1` with an ephemeral port.
4. It sends the API key, public JWK, loopback redirect URI, device name, and
   platform to the remote BFF activation endpoint.
5. The BFF verifies the API key and returns a short-lived authorization
   transaction and system-browser URL.
6. Wails opens that URL in the system browser.
7. The BFF performs Authorization Code + PKCE with the authorization service.
8. The OAuth callback terminates at the remote BFF. The BFF stores the OAuth
   access/refresh state server-side and redirects a one-time handoff code and
   matching state to the native loopback listener.
9. The native bridge exchanges the handoff code plus a device-key proof for an
   opaque native session reference bound to the device public-key thumbprint.
10. Subsequent calls present the opaque session and a fresh DPoP proof. The
    remote BFF resolves the server-side OAuth session, refreshes it when needed,
    authorizes the request, and proxies it to the appropriate Woca service.

The native bridge verifies authorization state, exact redirect URI, transaction
expiry, and single use. It never transfers browser cookies into the Wails
WebView.

### 5.3 Multiple devices

Each native installation generates its own non-exportable device private key.
A user may have multiple device records and sessions:

```text
user
├── device A key -> native session A
├── device B key -> native session B
└── browser profile -> Web session C
```

Sessions are revoked independently. Removing one device revokes only its
sessions. User disablement, tenant disablement, or "logout all" revokes every
session. A copied SQLite database is unusable without the original device key.
Reinstallation creates a new device identity.

### 5.4 Enterprise Web authorization

Enterprise Web uses a conventional remote BFF:

- The BFF is the OAuth client and performs Authorization Code + PKCE.
- OAuth tokens remain in the BFF's server-side session state.
- The browser receives only a hardened session cookie.
- The BFF enforces exact-origin CORS/CSRF policy and requires a custom header on
  state-changing API requests.
- The launcher may retain the enterprise URL and non-sensitive preferences,
  but never API keys, session cookies, or OAuth tokens in Web Storage.

## 6. Persistence

### 6.1 Native SQLite

The application database is `client.db` under the operating system's standard
per-user application data directory, not next to the executable or inside the
macOS `.app` bundle:

- macOS: `~/Library/Application Support/woca-native-client/client.db`
- Windows: `%AppData%/woca-native-client/client.db`
- Linux: `$XDG_DATA_HOME/woca-native-client/client.db`, falling back to a
  documented per-user data directory

The initial schema contains focused tables:

- `connection_profiles`: normalized enterprise origin, display name, active
  flag, creation and update timestamps.
- `preferences`: namespaced local key/value preferences.
- `native_sessions`: opaque session reference ciphertext, device ID, expiry,
  and last validation timestamp. It contains no OAuth access/refresh token.
- `schema_migrations`: applied schema version records.

Only one profile is active in the first milestone. The schema uses profile IDs
so multi-server profiles can be added later without a migration that rewrites
all local state.

### 6.2 Device key and local encryption

The device private key and the local envelope-encryption key live in the OS
secure-storage facility. The opaque native session is encrypted with
authenticated encryption before it is written to SQLite. Keys are never
hard-coded into the binary or stored in the same SQLite database as the
ciphertext.

If secure storage is unavailable or the key and database no longer match, the
client retains the non-sensitive URL/preferences, deletes the unusable local
session record, and requires authorization again.

### 6.3 Authorization PostgreSQL isolation

The remote authorization service uses a PostgreSQL data source isolated from
the Woca business/runtime PostgreSQL data source. Its authoritative records
include tenants, users, roles/scopes, API-key hashes, devices/public keys,
authorization grants, BFF sessions, encrypted OAuth token material or token
references, refresh-token families, revocations, and audit metadata.

No client connects directly to either PostgreSQL data source. The BFF and auth
service use separate least-privilege database credentials. The server-side
implementation must support per-device revocation and refresh-token reuse
detection.

## 7. Native Module Components

```text
native-client/
├── go.mod
├── go.sum
├── wails.json
├── main.go
├── app.go
├── build/
└── internal/
    ├── configstore/   SQLite schema, migrations, profiles, preferences
    ├── auth/          activation/authorization/handoff/session contracts
    ├── device/        device key and proof interfaces
    └── transport/     local /api to remote BFF HTTP/SSE transport
```

Responsibilities are intentionally narrow:

- `main.go`: embeds `webdist.DistFS()`, configures Wails v2.14.0, and wires
  dependencies.
- `app.go`: exposes only bootstrap state, connection submission, login start,
  logout, and connection-status operations to Vue.
- `configstore`: owns SQLite paths, schema, transactions, and persistence.
- `auth`: contains protocol-shaped interfaces and the native authorization
  state machine; no Vue concerns.
- `device`: abstracts secure key storage and DPoP/proof generation so platform
  implementations can be tested with an in-memory signer.
- `transport`: accepts only local `/api/*` traffic, resolves the active profile
  and session, adds the native session/proof, and forwards to the remote BFF.

The nested `go.mod` pins `github.com/wailsapp/wails/v2` to `v2.14.0`. It imports
the root module through a local `replace` to access `webdist`, while the package
dependency graph remains limited to the embedded Web assets rather than the
personal-edition executable.

## 8. Shared Web UI Integration

The existing `web/src/api/client.ts` keeps relative `/api` URLs. It must not
contain native server URLs, API keys, OAuth tokens, or DPoP logic.

A small native bootstrap layer is added under `web/src/native/`:

- Browser and personal builds immediately render the existing `App.vue`.
- Wails detects its bound native bridge and requests bootstrap state.
- An unconfigured native client renders an enterprise connection screen.
- A configured, recoverable session resumes through the Go bridge before the
  existing stores call `/api`.
- A session that requires authorization shows the login action while retaining
  the configured URL.
- A local path, open, or migration failure returns `fatal_local_data`; the gate
  shows fixed localized recovery guidance and the database path when known,
  without mounting the application or connection screen.
- Once ready, the unchanged Woca application shell is mounted.

This gate prevents the current `App.vue` startup hooks from sending API calls
before the native transport has an active connection.

## 9. Remote Transport

Wails `assetserver.Options` uses `webdist.DistFS()` for static assets and an
`http.Handler` for `/api` requests. Wails calls the handler for every non-GET
request and for GET paths that do not exist in the asset filesystem, which
covers the API surface without changing frontend URLs.

The transport must:

- Preserve HTTP method, escaped path, query, request body, response status,
  content type, and SSE streaming/flushing behavior.
- Reject GET paths in the decoded `/api` namespace that end in `/` or
  `/index.html` with a fixed non-404 JSON error before any asset, profile,
  credential, or upstream work. Wails v2.14 otherwise treats those paths as
  application documents, replacing handler 404s or injecting runtime scripts
  into 200 HTML, and `path.Clean`-resolved traversal aliases such as
  `/api/../index.html` serve the embedded application without reaching the
  transport. The rejection runs as an asset-server middleware ahead of Wails'
  document handling and repeats inside the transport handler.
- Propagate request cancellation and set bounded connect/header timeouts while
  allowing long-lived SSE response bodies.
- Reject non-`/api/` paths.
- Normalize and pin the selected enterprise origin.
- Require HTTPS except for `localhost`, `127.0.0.1`, and `[::1]` development
  origins.
- Strip hop-by-hop headers and all client-supplied authentication, cookie, and
  forwarding headers before adding trusted native-session credentials.
- Refuse redirects to a different origin.
- Return uniform JSON errors for unavailable, unauthorized, and malformed
  upstream responses without leaking session material.

The first scaffold supplies a test signer/session provider to prove transport
behavior. Production DPoP is activated only when the corresponding remote BFF
contract exists; no production fallback sends raw API keys as Bearer tokens.

## 10. Development and Build Workflow

`native-client/wails.json` uses `../web` as its frontend directory and the
existing npm install/build commands. Production builds therefore run the same
Vue type-check and Vite build used by the personal edition, then compile those
assets into the Wails executable.

Development provides an explicitly development-only connection override under
the Wails `dev` build tag. It may target a loopback test BFF so the full Woca UI
can be exercised before the production authorization service exists. The
override is absent from production builds and cannot disable HTTPS or
authentication for non-loopback origins.

Expected developer commands are:

```bash
cd native-client
wails dev
wails build
```

The repository Makefile will expose native convenience targets without
changing the existing `all`, `build`, `build-personal`, or `serve` semantics.

## 11. Error and Recovery Behavior

- Invalid URL: remain on the connection screen and explain the required
  absolute HTTPS origin.
- Enterprise host unavailable: retain the profile and offer retry/edit; do not
  enter the main UI with a half-configured transport.
- Invalid or revoked API key: discard it immediately and show the server's
  sanitized activation failure.
- Authorization cancelled or timed out: close the loopback listener, invalidate
  the transaction, and allow retry.
- Invalid callback state or reused handoff: reject it and require a new flow.
- Session expired/revoked: clear only the local encrypted session, preserve URL
  and preferences, and return to login.
- Missing device key: invalidate the local session and register a new device
  during the next authorization.
- Upstream 401/403: do not retry mutating requests blindly; reconcile session
  status with the BFF and return to login when revoked.
- SSE disconnect: preserve the current Web UI reconnection semantics while the
  Go transport streams and flushes frames without buffering the entire body.
- SQLite migration/open failure: show a fatal local-data error with the database
  path; never silently replace a readable but incompatible database.

## 12. Testing Strategy

Implementation follows test-driven development.

### 12.1 Go tests

- SQLite path selection, migration idempotence, profile persistence, atomic
  active-profile changes, and session removal without preference loss.
- URL normalization and HTTPS/loopback policy.
- Header stripping and trusted credential injection.
- Method/path/query/body/status preservation through an `httptest` BFF.
- SSE frames arrive incrementally rather than after upstream close.
- Cancellation closes the upstream request.
- Cross-origin redirect refusal.
- Bootstrap state transitions: unconfigured, authorization required, ready,
  revoked, and local-key loss.

### 12.2 Web tests

- Browser/personal mode bypasses the native connection gate.
- Native unconfigured mode renders URL/API-key inputs without persisting the
  API key in browser storage.
- The main Woca app does not initialize stores before native readiness.
- Successful readiness mounts the existing app shell.
- Logout/session loss returns to login while preserving the URL.

### 12.3 Build and runtime verification

- Run the complete root Go test suite to detect personal-edition regressions.
- Run Vue type-check and production build.
- Run all nested native-client Go tests.
- Run `wails build` from `native-client/` and verify the generated application
  uses Wails v2.14.0.
- Launch the macOS application against a loopback test BFF and visually verify
  that the existing Woca Web UI renders and issues HTTP/SSE requests through
  the native transport.
- Confirm the personal `woca` dependency baseline and build remain unchanged by
  the nested module.

## 13. First-Milestone Acceptance Criteria

The Wails scaffold milestone is complete only when all of the following are
proven from the current worktree:

1. `native-client/` is an independent Go module pinned to Wails v2.14.0.
2. It builds the existing `web/` project rather than a copied frontend.
3. A production Wails build embeds `webdist.DistFS()` and succeeds.
4. Launching the application opens a native window containing the Woca Web UI.
5. An unconfigured production client shows the enterprise URL/API-key bootstrap
   screen; it never asks for a URL in personal/browser mode.
6. URL and non-sensitive native preferences survive a Wails restart in SQLite.
7. The API key is not persisted by SQLite, browser storage, logs, or generated
   files.
8. `/api` HTTP and SSE traffic passes through the tested native transport to a
   configured loopback test BFF.
9. The existing personal binary and Web UI build/tests still pass and do not
   acquire Wails dependencies.
10. The production code contains no direct-to-service or raw-API-key Bearer
    fallback that bypasses the agreed remote BFF architecture.

## 14. Later Server-Side Milestones

The following are required for the complete enterprise authentication system
but are deliberately not claimed by the first Wails scaffold milestone:

- Remote BFF activation, authorization, callback, handoff, native-session,
  logout, and device-management endpoints.
- Authorization service and its isolated PostgreSQL schema/data source.
- API-key issuance/revocation administration.
- System-browser Authorization Code + PKCE integration.
- Device secure-storage implementations and production DPoP proofs.
- Native opaque-session issuance and proof verification.
- Web BFF cookie session, CSRF controls, and server-side OAuth token lifecycle.
- Multi-device listing, individual revocation, logout-all, audit, and refresh
  replay detection.

These later milestones must preserve the client boundaries in this document:
both Enterprise Web and native call only the remote BFF, and OAuth access or
refresh tokens never reach the Vue application.

## 15. Standards and Framework References

- [Wails v2.14.0 release](https://github.com/wailsapp/wails/releases/tag/v2.14.0)
- [Wails application assets](https://wails.io/docs/guides/application-development/)
- [RFC 8252: OAuth 2.0 for Native Apps](https://www.rfc-editor.org/info/rfc8252/)
- [RFC 9449: OAuth 2.0 Demonstrating Proof of Possession](https://www.rfc-editor.org/info/rfc9449/)
- [RFC 9700: Best Current Practice for OAuth 2.0 Security](https://www.rfc-editor.org/info/rfc9700/)
- [RFC 10017: OAuth 2.0 for Browser-Based Applications](https://www.rfc-editor.org/rfc/rfc10017.html)
