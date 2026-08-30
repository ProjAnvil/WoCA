# woca per-role distribution packages

One tar per microservice role: the role's binary plus operator-ready
start/stop scripts with the role name **baked in** — ops staff run
`./start.sh` (macOS/Linux) or `.\start.ps1` (Windows) with no arguments;
the binary is launched in the background and its pid is recorded. The
package is what you deliver into a Docker container: COPY or volume-mount
the tar, extract it there, and run the script.

## Build

```bash
make woca-ops                    # one role (name is substituted into the scripts)
make dist-micro                  # every role
GOOS=linux GOARCH=amd64 make woca-worker   # cross-compile for containers (CGO-free)
GOOS=windows GOARCH=amd64 make woca-edge   # windows package: woca-edge.exe + .ps1 scripts
```

Output: `build/dist/<role>-<version>-<os>-<arch>.tar.gz` (+ `.sha256`).

## Package layout

The script pair is chosen by build target: macOS/Linux builds carry the
shell pair, Windows builds (`GOOS=windows`) carry the PowerShell pair
**instead** — with the `.exe` binary.

```text
woca-ops/                       (macOS / Linux)
├── woca-ops         the role binary
├── start.sh         background start (nohup, pid file)
├── stop.sh          stop (pid first, name fallback)
├── VERSION          git describe of the build
└── README.md        this file

woca-ops/                       (Windows)
├── woca-ops.exe     the role binary
├── start.ps1        background start (Start-Process hidden, pid file)
├── stop.ps1         stop (pid first, name fallback)
├── VERSION / README.md
```

Run artifacts created beside the scripts: `woca-ops.pid` (the live pid),
`woca-ops.log` (stdout/stderr), and on Windows additionally
`woca-ops.err.log`.

## Using the package in a container

Dockerfile form (bake the package into the image):

```dockerfile
FROM debian:bookworm-slim
COPY woca-ops-*.tar.gz /opt/
RUN tar -xzf /opt/woca-ops-*.tar.gz -C /opt && rm /opt/woca-ops-*.tar.gz
# configuration arrives as container env (WOCA_DB, WOCA_NACOS_URL, ...)
# or as /opt/woca-ops/service.env mounted next to the scripts
ENTRYPOINT ["/opt/woca-ops/start.sh"]
```

Volume form (the package is passed in, extracted at container start):

```bash
docker run -d --name woca-ops \
  -v $PWD/woca-ops-1.2.3-linux-amd64.tar.gz:/pkg/pkg.tar.gz \
  -e WOCA_DB=postgres://... -e WOCA_NACOS_URL=http://nacos:8848/nacos \
  debian:bookworm-slim \
  bash -c 'mkdir -p /opt && tar -xzf /pkg/pkg.tar.gz -C /opt && exec /opt/woca-ops/start.sh'
```

For a long-lived interactive container, exec in and use `start.sh` /
`stop.sh` directly — that is the point of the scripts: nobody has to
know how to launch a Go binary.

## Start / stop semantics

- **start** refuses double starts (live-pid check with a cmdline guard
  against pid reuse), sources an optional `service.env` (KEY=value;
  environment values already set win), launches with `nohup … &`
  (bash) / `Start-Process -WindowStyle Hidden` (PowerShell), writes the
  pid file, and verifies after one second that the process is still
  alive — early exits print the log tail.
- **stop** is pid-first: SIGTERM, wait up to `STOP_TIMEOUT` (default 45s,
  covering the edge role's 40s SSE grace), then SIGKILL; the outcome is
  printed. If the pid file is missing or its pid is gone, it **falls
  back to searching for the binary by name** (`pgrep` / `Get-Process`),
  so a wiped pid file never leaves an orphan behind.
- Windows note: console processes have no SIGTERM contract;
  `stop.ps1` uses `Stop-Process`.

## Configuration

Roles read their `WOCA_*` configuration from the environment. Inside
containers, prefer passing `-e` / compose `environment:`; the optional
`service.env` file (sourced by both start scripts) covers non-container
hosts — environment values already present win over file values.
