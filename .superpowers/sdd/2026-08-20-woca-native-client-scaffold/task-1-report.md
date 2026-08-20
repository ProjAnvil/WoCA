# Task 1 Report: Nested native module and shared asset contract

## Implementation

- Created the isolated `native-client` Go module with the exact required module path, Go version, direct dependencies, and replacement for the root module.
- Added `frontendAssets() fs.FS`, delegating directly to `webdist.DistFS()`.
- Added the shared-assets test asserting that `index.html` is available.
- Added the exact Wails project configuration targeting `../web` and `../web/dist`.
- Generated `native-client/go.sum`.

## Files

- `native-client/go.mod`
- `native-client/go.sum`
- `native-client/assets.go`
- `native-client/assets_test.go`
- `native-client/wails.json`

## Commands and results

### RED evidence

Command:

```bash
cd native-client && go test .
```

Result: failed as intended with:

```text
./assets_test.go:9:12: undefined: frontendAssets
FAIL
```

### GREEN evidence

Command:

```bash
cd native-client && go mod tidy && go test .
```

Result: `go test .` passed:

```text
ok github.com/projanvil/woca/native-client 0.525s
```

Because `wails` and `modernc.org/sqlite` are not yet imported by production code, `go mod tidy` removed those explicitly required direct dependencies. The exact manifest required by the brief was restored, then `go mod download` generated `go.sum`; a fresh test passed and Wails resolved correctly:

```bash
go mod download && go test .
```

```text
ok github.com/projanvil/woca/native-client (cached)
```

```bash
go list -m -f '{{.Path}} {{.Version}}' github.com/wailsapp/wails/v2
```

```text
github.com/wailsapp/wails/v2 v2.14.0
```

### Isolation evidence

Command:

```bash
go list -m -f '{{.Path}} {{.Version}}' github.com/wailsapp/wails/v2
cd ..
go list -m all | rg 'wailsapp/wails' || true
```

Result: the nested module reported `github.com/wailsapp/wails/v2 v2.14.0`; the root module produced no matching Wails output.

## Self-review

- The nested module has no changes to the root module.
- The asset adapter has no duplicated embedding or asset transformation and returns the shared `webdist.DistFS()` filesystem directly.
- The test checks both non-nil assets and the required `index.html` entry.
- Wails configuration values match the brief exactly.
- `go test .` passes in the nested module after dependency resolution.

## Concerns

- `go mod tidy` currently removes the required but not-yet-imported Wails and SQLite direct dependencies. The committed manifest intentionally preserves the exact direct dependency boundary from the brief; future native implementation imports should make `go mod tidy` retain them naturally.
