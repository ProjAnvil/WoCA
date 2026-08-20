# Woca Enterprise native client

This directory contains the enterprise-only Wails desktop client. It is an
independent Go module that embeds the existing `../web` application; it is not
part of the personal-edition `woca` binary or its default build targets.

## Prerequisites and commands

Install the Wails CLI at the version pinned by this module:

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@v2.14.0
wails version
```

From the repository root, use:

```bash
make native-test
make native-build
make native-dev
```

`native-build` first builds the shared Vue application in `web/dist`, then
creates a clean Wails artifact. The generated binary is written below
`native-client/build/bin/`, which is intentionally ignored while Wails source
icons and platform metadata below `native-client/build/` stay tracked.

## Production boundary

On first launch, an unconfigured production native client presents the
enterprise server URL and one-time API-key activation screen. The API key is
kept only long enough to submit activation and is never stored in SQLite,
browser storage, logs, or generated files. SQLite stores only local,
non-sensitive configuration such as the remote BFF URL and preferences.

The production client is fail-closed in this milestone. Remote BFF activation,
system-browser Authorization Code with PKCE, one-time handoff, DPoP-bound native
sessions, secure device-key storage, and logout/device management belong to a
later server-side milestone. OAuth access and refresh tokens must never enter
the Vue application or be persisted by this native client. See the approved
[native-client design](../docs/superpowers/specs/2026-08-20-woca-native-client-design.md).

Wails routes bound-call arguments, callback results, frontend `runtime.Log*`
text, and internal diagnostics through the same public logger interface. Since
their origins cannot be distinguished at that boundary, the application never
delegates any Wails message body: every method emits one fixed constant at the
original level. This also preserves the delegate's fatal termination semantics.
The deliberate diagnostics tradeoff prevents API keys, URLs, database paths,
arguments, callbacks, frontend text, or error details from reaching logs.
Top-level startup failures still use Go's standard logger; bound bridge calls
cannot reach that path, and startup errors contain no bridge/API-key input.

Wails 2.14's single-instance lock is a best-effort desktop UX guard, not a data
integrity mutex. SQLite is the preference-ordering authority. Schema version 2
maintains a persistent epoch counter and records the applied `(epoch, revision)`
for each key. Opening, migrating, and reading a Store do not claim ownership;
the Store atomically claims an epoch only on its first versioned preference
write. Every write then checks both values in one immediate transaction, so an
actual later writer fences older writers while a rejected/read-only launch does
not. Controlled shutdown still stops new preference calls and drains accepted
calls before closing SQLite, but data safety does not depend on the Wails lock
or graceful shutdown.

## Local database recovery

If the native client cannot resolve, open, or migrate `client.db`, it opens a
fatal local-data screen instead of silently replacing or repairing the
database. When available, the screen shows the exact database path but never
renders raw SQLite errors. Quit Woca, restore compatible file permissions or a
supported schema (or recover the database from a known-good backup), and then
relaunch the application. If the path itself is unavailable, repair the user
home or platform application-data environment before relaunching. The failed
process does not offer an in-place retry because its Store was never opened.

## Loopback UI and transport development

For local visual development, first build and run the personal Woca service:

```bash
make build
./build/woca serve --addr 127.0.0.1:8899 --no-browser
```

In another terminal, start Wails with the development-only override:

```bash
WOCA_NATIVE_DEV_BFF_URL=http://127.0.0.1:8899 make native-dev
```

The loopback Woca service is only a UI/HTTP/SSE transport development fixture.
It is not the enterprise production authentication path and the override is
excluded from production builds. Enterprise Web and native production clients
must both communicate only with the remote BFF.
