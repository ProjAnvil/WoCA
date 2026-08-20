# Woca Native Client Authentication Milestone (M2) Implementation Plan

> **For agentic workers:** Use superpowers:subagent-driven-development or
> superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Make the production native client able to connect to a remote
enterprise BFF: activate an API key into a device-bound opaque native session,
authenticate every proxied request with a DPoP proof, and revoke the session on
logout — without any OAuth token, API key, or DPoP private key ever entering
Vue code, SQLite plaintext, or logs.

**Architecture:** The enterprise gateway (already carrying Phase-1
`internal/tenant` API-key authn and PG RLS) gains a small native-auth surface:
`activate` exchanges an API key plus a device-generated DPoP JWK for an opaque
session id; `session` middleware then authenticates `Authorization: DPoP
<opaque-id>` plus a per-request DPoP proof JWT (RFC 9449: htm/htu/iat/jti/ath,
ES256, thumbprint pinned at activation). The client implements the real
`auth.Activator`, stores the session secret AES-256-GCM-encrypted under a
device key held in the OS keychain, and the `transport.CredentialProvider`
mints proofs per request. The Wails bridge keeps passing only booleans and
URLs to Vue.

**Spec:** `docs/superpowers/specs/2026-08-20-woca-native-client-design.md`
(§12 transport contract, §14 later-milestone boundaries, §13 constraints that
remain in force).

## Global Constraints (carried from the design spec)

- OAuth access/refresh tokens and DPoP private keys never enter Vue.
- The API key crosses the Wails bridge exactly once per activation and is
  never persisted, logged, or forwarded to any endpoint other than activate.
- SQLite stores only URL, preferences, and AES-GCM session ciphertext.
- Production HTTPS-only (loopback HTTP remains dev-only under the `dev` tag).
- Enterprise clients call only the BFF; no direct-to-service path, no raw
  API-key Bearer fallback (`productionCredentials` stays fail-closed until the
  real provider replaces it).
- Personal edition (`make`, `make build`, `make serve`) must not gain any of
  these dependencies.

---

## Task 1: Server — native session store on the tenant schema

**Files:** enterprise gateway tree; new `internal/tenant/session.go` +
`session_test.go` (PG-backed, RLS-scoped).

- [ ] Migration: `native_sessions` table (id hash, tenant, user, device_id,
  dpop_jwk_thumbprint, expires_at, last_validated_at, revoked_at NULL) with
  tenant RLS policies mirroring existing tables.
- [ ] `IssueSession(ctx, tenantID, userID, deviceID, thumbprint, ttl)` →
  returns opaque id + secret (secret shown once, like IssueKey).
- [ ] `LookupSession(ctx, idHash)` / `RevokeSession(ctx, idHash)` /
  `TouchSession`; id is stored hashed (SHA-256) so DB reads cannot mint auth.
- [ ] Tests: issue/lookup/revoke/expiry, hash never reversible, RLS on.

## Task 2: Server — activate + logout + validate endpoints

**Files:** gateway handlers `nativeauth` (+ tests over `httptest`).

- [ ] `POST /api/native/activate`: JSON `{api_key, device_id, dpop_jwk}` →
  `tenant.APIKeyProvider.Authenticate` → validate JWK (EC P-256, no private
  material) → `IssueSession` → `{session_id, session_secret, expires_at}`.
  Rate-limit per prefix; constant-time failures; API key never logged.
- [ ] `GET /api/native/session`: validates the caller's session (middleware
  auth), returns `{expires_at, device_id}` for client startup checks.
- [ ] `DELETE /api/native/session`: revokes (logout). Requires DPoP like all
  session-authed routes.
- [ ] Tests: happy path, wrong key, weak JWK, revoked key, replayed id.

## Task 3: Server — DPoP session middleware

**Files:** `internal/platform/authn` extension or new `internal/platform/dpop`
(+ tests).

- [ ] Verify `Authorization: DPoP <id>` against session lookup (hash id).
- [ ] Verify `DPoP` proof JWT: ES256, `htm`/`htu` match the request (origin
  normalization identical to `configstore.NormalizeBaseURL`), `iat` window
  ±30s with replay cache on `jti`, `ath` = base64url(SHA-256(session id)),
  JWK thumbprint equals the one pinned at activation.
- [ ] Failures → 401 JSON `{"error":"authorization required"}`; no detail
  leakage. Wire into the gateway auth chain after API-key authn.
- [ ] Tests: proof reuse (jti), mismatched htu/htm/ath, wrong thumbprint,
  expired iat, revoked session.

## Task 4: Client — device key + session persistence

**Files:** `native-client/internal/devicekey/` (Keychain/secret-store with
file-0600 fallback behind an interface), extend `configstore` session table
helpers, tests.

- [ ] `devicekey.GetOrCreate(deviceID)` — macOS Keychain v1 (security
    framework via `keychain` CGO or `security` CLI subprocess — decide by
    dependency budget; fallback: `~/Library/Application Support/…/device.key`
    0600 + document).
- [ ] AES-256-GCM wrap/unwrap of the session secret; ciphertext + expiry +
  device id into `native_sessions` (schema from M1 is already there).
- [ ] Tests: roundtrip, tampered ciphertext fails, key rotation invalidates.

## Task 5: Client — real activator + DPoP credential provider

**Files:** `native-client/internal/auth/activator_http.go`,
`native-client/internal/transport/credentials_dpop.go` (+ tests, httptest).

- [ ] HTTP Activator: POST activate, TLS-only (reuse loopback policy), zero
  logging of the key, returns `Ready` on 200 and typed errors otherwise.
- [ ] DPoP credentials: per-request ES256 proof (htm/htu/iat/jti/ath),
  private key from devicekey, session from configstore; 401 with
  `use_dpop_nonce`-style retry handled when added by the server later.
- [ ] Wire into `runWith`: production build now uses these instead of
  `UnavailableActivator`/`productionCredentials`; dev build unchanged
  (empty credentials for the loopback fixture). `main_test.go` updated.

## Task 6: Bridge + Vue flow, end-to-end verification

**Files:** `bridge/service.go` (session-aware Bootstrap/Logout), i18n copy,
`web` tests, README.

- [ ] `Connect` → activate → persist session → `ready` on success; failure
  keeps `authorization_required` + sanitized error (no key material).
- [ ] `Logout` → revoke at BFF, delete local session, return
  `authorization_required`.
- [ ] Bootstrap with valid stored session → `ready` without asking again;
  expired/revoked → `authorization_required`.
- [ ] End-to-end against a locally deployed gateway (docker-compose PG):
  activate → proxied chat run over SSE with DPoP → restart reuses session →
  logout revokes. Screenshot evidence as in M1; full test suites green.

---

**Out of scope (later milestones):** system-browser PKCE handoff, web BFF
cookie sessions/CSRF, multi-device management UI, refresh replay detection,
Windows/Hello secure storage.
