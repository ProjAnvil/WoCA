# Contributing to woca

Thanks for your interest in improving woca! This document covers the basics
of building, testing, and submitting changes.

## Prerequisites

- Go ≥ 1.26
- Node.js ≥ 20 (only for web frontend work)
- A model provider API key (Anthropic-compatible or OpenAI-compatible), or a
  local gateway

## Build & test

```bash
make build          # go build ./...
make test           # go test ./...
make web            # build the web frontend into web/dist (embedded via go:embed)
make serve          # build web + binary, then run `woca serve`
```

After changing anything under `web/src`, run `make web` before building the
Go binary — the frontend is embedded at compile time.

## Code style

- Comments, identifiers, and documentation are in **English**.
- Keep comments about *why*, not *what*.
- `gofmt`-clean code; CI runs `go vet` and the race detector, so run
  `go test -race ./...` for anything touching concurrency.
- New behavior comes with tests. The project keeps high unit coverage on
  purpose — most packages have table-driven tests next to the code.
- Commit messages follow [Conventional Commits]
  (`feat:`, `fix:`, `docs:`, `test:`, `refactor:`, …).

## Pull requests

1. Fork, create a feature branch.
2. Make the change with tests.
3. Ensure `gofmt -l .`, `go vet ./...`, and `go test ./...` are clean.
4. Open a PR describing *what* changed and *why*.

For bug reports, include the woca version (`woca --version`), the surface
you were using (one-shot / TUI / web), and — if relevant — an observability
log captured with the `debug-log` setting enabled.

[Conventional Commits]: https://www.conventionalcommits.org/
