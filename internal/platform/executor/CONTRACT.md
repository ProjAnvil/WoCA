# Woca Sandbox HTTP Contract v2

This document is the normative wire specification for sandbox backends
adapted to the woca `Executor` interface (`executor.go`). Any HTTP service
implementing these endpoints can be driven by `NewSandbox` as a sandbox
executor. Version 2 of the contract; see "Versioning" at the end for the
v1 → v2 delta.

All examples assume the service is rooted at `http://sandbox.local` and the
sandbox instance ID is `sbx_1`.

## 1. Common request rules

- Every request carries the header `X-Woca-Sandbox-Contract: 2`.
- When the service requires authentication, clients send
  `Authorization: Bearer <key>`; the header is omitted when no key is
  configured. Servers treat a missing header as anonymous.
- JSON request/response bodies use `Content-Type: application/json`.
- Binary payloads (stdout, stderr, file content, stdin, grep line text) are
  **base64** (standard encoding) inside JSON strings, so frames stay
  binary-safe end to end.
- `env` is a JSON **object** (`{"K":"V"}`), never an array.

## 2. Endpoint table (15)

| # | Operation | Method & path |
|---|---|---|
| 1 | Create sandbox | `POST /sandbox/create` |
| 2 | Exec (synchronous) | `POST /sandbox/{id}/exec` |
| 3 | Start long process | `POST /sandbox/{id}/start` |
| 4 | Read process output | `GET /sandbox/{id}/sessions/{sid}/output?offset=N` |
| 5 | Write process input | `POST /sandbox/{id}/sessions/{sid}/input` |
| 6 | Kill process | `POST /sandbox/{id}/sessions/{sid}/kill` |
| 7 | Read file | `GET /sandbox/{id}/read?path=` |
| 8 | Write file | `POST /sandbox/{id}/write` |
| 9 | Stat | `GET /sandbox/{id}/stat?path=` |
| 10 | Mkdir | `POST /sandbox/{id}/mkdir` |
| 11 | Glob | `POST /sandbox/{id}/glob` |
| 12 | Grep | `POST /sandbox/{id}/grep` |
| 13 | Upload archive | `PUT /sandbox/{id}/archive` |
| 14 | Download archive | `GET /sandbox/{id}/archive` |
| 15 | Destroy sandbox | `DELETE /sandbox/{id}` |

## 3. Endpoints in detail

### 3.1 Create — `POST /sandbox/create`

Request body:

```json
{"cwd": "/workspace"}
```

`cwd` requests the sandbox working directory; backends that cannot honor it
may ignore the field, but the archive endpoints (§3.13/3.14) always unpack
relative to `/workspace`.

Response:

```json
{"sandbox_id": "sbx_1"}
```

### 3.2 Exec — `POST /sandbox/{id}/exec`

Runs a command to completion and returns the aggregated result.

```bash
curl -X POST http://sandbox.local/sandbox/sbx_1/exec \
  -H 'Content-Type: application/json' \
  -H 'X-Woca-Sandbox-Contract: 2' \
  -H 'Authorization: Bearer <key>' \
  -d '{"argv":["echo","hi"],"cwd":"/workspace","env":{"FOO":"bar"},
       "stdin":"","timeout_ms":30000}'
```

Request fields:

| Field | Type | Meaning |
|---|---|---|
| `argv` | string[] | argv array; `argv[0]` is the command, resolved via `PATH` when not a path |
| `cwd` | string | working directory; empty means the sandbox default |
| `env` | object | environment overrides applied on top of the sandbox default environment; `{}` inherits everything |
| `stdin` | string | base64 bytes piped to stdin; empty string = no stdin |
| `timeout_ms` | int | wall-clock budget for the command; `0` or omitted means the backend default |

Response:

```json
{"exit_code": 0, "stdout": "aGkK", "stderr": ""}
```

`stdout`/`stderr` are base64. A non-zero `exit_code` is a normal result,
**not** an HTTP error.

### 3.3 Start — `POST /sandbox/{id}/start`

Launches a long-running process and returns a session handle immediately.

```bash
curl -X POST http://sandbox.local/sandbox/sbx_1/start \
  -H 'Content-Type: application/json' \
  -d '{"argv":["cat"],"cwd":"/workspace","env":{}}'
```

Request fields: same as exec minus `stdin`/`timeout_ms` (stdin is streamed
via §3.5; lifetime is governed by kill).

Response:

```json
{"session_id": "ses_1"}
```

### 3.4 Output — `GET /sandbox/{id}/sessions/{sid}/output?offset=N`

Polling read with a byte cursor. `offset=0` starts from the beginning.

```bash
curl 'http://sandbox.local/sandbox/sbx_1/sessions/ses_1/output?offset=0'
```

Response:

```json
{
  "events": [
    {"stream": "stdout", "data": "aGkK"},
    {"stream": "stderr", "data": ""}
  ],
  "done": true,
  "exit_code": 0,
  "next_offset": 4
}
```

| Field | Type | Meaning |
|---|---|---|
| `events` | array | output since `offset`; `stream` is `"stdout"` or `"stderr"`; `data` is base64; empty array when nothing new |
| `done` | bool | process has exited; `exit_code` is final |
| `exit_code` | int | meaningful when `done`; may be `-1` when killed |
| `next_offset` | int | cursor for the next poll |

Events must be returned in production order; per-stream ordering within the
process lifetime is preserved.

### 3.5 Input — `POST /sandbox/{id}/sessions/{sid}/input`

```bash
curl -X POST http://sandbox.local/sandbox/sbx_1/sessions/ses_1/input \
  -H 'Content-Type: application/json' \
  -d '{"data":"aGkK"}'
```

`data` is base64 bytes appended to the process stdin. Writing after process
exit is a no-op (200), not an error. EOF is signaled with an explicit frame:

```json
{"eof": true}
```

On receiving `{"eof":true}` the backend closes the process stdin; data
frames and the EOF frame are mutually exclusive (a data frame carries
`{"data":"<base64>","eof":false}`).

### 3.6 Kill — `POST /sandbox/{id}/sessions/{sid}/kill`

```bash
curl -X POST http://sandbox.local/sandbox/sbx_1/sessions/ses_1/kill \
  -H 'Content-Type: application/json' -d '{}'
```

Terminates the process. Subsequent output polls return `done: true` with the
final `exit_code` (`-1` is acceptable for killed processes). Killing an
already-exited session is a no-op (2xx).

### 3.7 Read file — `GET /sandbox/{id}/read?path=`

```bash
curl 'http://sandbox.local/sandbox/sbx_1/read?path=%2Fworkspace%2Fmain.go'
```

`path` is URL-encoded. Response:

```json
{"content": "cGFja2FnZSBtYWluCg=="}
```

`content` is base64. Missing file → 404 with `{"error":"..."}` (§5).

### 3.8 Write file — `POST /sandbox/{id}/write`

```bash
curl -X POST http://sandbox.local/sandbox/sbx_1/write \
  -H 'Content-Type: application/json' \
  -d '{"path":"/workspace/main.go","content":"cGFja2FnZSBtYWluCg==","mode":420}'
```

| Field | Type | Meaning |
|---|---|---|
| `path` | string | absolute path inside the sandbox |
| `content` | string | base64 bytes |
| `mode` | int | Unix permission bits as a decimal number (e.g. `420` = `0644`, `493` = `0755`); omitted/0 → backend default |

Parent directories are created as needed. Response: `204 No Content` (or
`200 {}`).

### 3.9 Stat — `GET /sandbox/{id}/stat?path=`

```bash
curl 'http://sandbox.local/sandbox/sbx_1/stat?path=%2Fworkspace'
```

Response:

```json
{"mode": 420, "size": 12, "mtime_ms": 0, "is_dir": false}
```

| Field | Type | Meaning |
|---|---|---|
| `mode` | int | Unix permission bits as decimal (same encoding as write) |
| `size` | int64 | file size in bytes (0 for dirs) |
| `mtime_ms` | int64 | modification time, unix milliseconds |
| `is_dir` | bool | directory flag |

Missing path → 404.

### 3.10 Mkdir — `POST /sandbox/{id}/mkdir`

```bash
curl -X POST http://sandbox.local/sandbox/sbx_1/mkdir \
  -H 'Content-Type: application/json' \
  -d '{"path":"/workspace/pkg/sub"}'
```

MkdirAll semantics: all missing parents are created. Permission bits are
backend-defined (the Executor surface has no perm here); the request carries
only `path`. Response: `204`.

### 3.11 Glob — `POST /sandbox/{id}/glob`

```bash
curl -X POST http://sandbox.local/sandbox/sbx_1/glob \
  -H 'Content-Type: application/json' \
  -d '{"pattern":"/workspace/**/*.go"}'
```

`pattern` follows Go `path.Match`/`filepath.Match` shape as produced by the
caller. Response:

```json
{"matches": ["/workspace/a.go", "/workspace/b.go"]}
```

`matches` are absolute sandbox paths, empty array when nothing matches.

### 3.12 Grep — `POST /sandbox/{id}/grep`

```bash
curl -X POST http://sandbox.local/sandbox/sbx_1/grep \
  -H 'Content-Type: application/json' \
  -d '{"root":"/workspace","pattern":"TODO","max_matches":200}'
```

| Field | Type | Meaning |
|---|---|---|
| `root` | string | absolute directory to walk |
| `pattern` | string | Go regexp source (e.g. `TODO|FIXME`) |
| `max_matches` | int | hard cap on returned matches; the standard client always sends 200 |

Walk semantics mirror the Executor contract: hidden directories (name
starting with `.`) are skipped, except `root` itself. Response:

```json
{"matches": [{"path": "/workspace/a.go", "line": 3, "text": "VE9ETwo="}]}
```

`text` is the full matching line, **base64**-encoded (binary-safe). `line` is
1-based.

### 3.13 Upload archive — `PUT /sandbox/{id}/archive`

```bash
curl -X PUT http://sandbox.local/sandbox/sbx_1/archive \
  -H 'Content-Type: application/gzip' \
  --data-binary @workspace.tar.gz
```

Body is a **gzip-compressed tar** stream. See §4 for tar conventions. On
success the workspace is replaced (backend-defined whether atomically);
response `204`.

### 3.14 Download archive — `GET /sandbox/{id}/archive`

```bash
curl http://sandbox.local/sandbox/sbx_1/archive -o workspace.tar.gz
```

Response body is a gzip-compressed tar stream of the sandbox workspace
(`Content-Type: application/gzip`).

### 3.15 Destroy — `DELETE /sandbox/{id}`

```bash
curl -X DELETE http://sandbox.local/sandbox/sbx_1
```

Destroys the sandbox and all sessions. Destroying an unknown or already
destroyed id returns 2xx (idempotent). Response: `204`.

## 4. tar conventions

- Archive entries are paths **relative to the workspace root** (no leading
  `/`, no `./` prefix). `a.go`, `pkg/sub/b.go`.
- Entry permission bits are preserved (`hdr.Mode`).
- Symbolic links are preserved (`Typeflag == TypeSymlink` with `Linkname`).
- The default exclude set is `.git` (client side); backends may enforce the
  same exclusion on download.
- Unpack target inside the sandbox is `/workspace`.
- Directory entries (`Typeflag == TypeDir`) may be present and should be
  preserved on round-trip when the backend tracks directories.

## 5. Error semantics

- Any failure is a non-2xx status with a JSON body:

  ```json
  {"error": "sandbox sbx_1 not found"}
  ```

  Clients surface the message verbatim where possible.
- Unknown sandbox id or session id → `404`.
- Malformed request bodies → `400`.
- Backends SHOULD implement a sandbox TTL as a safety net: sandboxes idle
  beyond the TTL are destroyed server-side even if the client never calls
  destroy, so leaked sandboxes cannot accumulate.

## 6. Encoding pitfalls (for adapter authors)

- `stdout`, `stderr`, `stdin`, file `content`, session `input.data`,
  output event `data`, and grep `text` are base64 — never raw strings.
- `env` is an object; v1's env array (and the v1 `command` joined string)
  are gone.
- `mode` fields are decimal Unix permission bits (e.g. `0644` → `420`),
  not octal strings and not Go `fs.FileMode` flag bits.

## 7. Versioning

- v1 (Phase 3): `POST /sandbox/{id}/exec` with `{"command": "joined string"}`,
  no cwd/env/stdin, plain-text stdout/stderr, Start/Stat/Mkdir/Grep/archive
  unsupported. Superseded by v2.
- v2 (this document): the `X-Woca-Sandbox-Contract: 2` header on every
  request; argv/cwd/env/stdin/timeout_ms exec; start/output/input/kill
  session surface; full file surface; tar.gz archive round-trip.
