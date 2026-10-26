.PHONY: all build build-relay e2e-relay build-personal test race web-test ee-web-test test-ui vet fmt run clean lint check check-sse tui serve web ee-web install deps-baseline check-deps native-test native-build native-dev ent-up ent-down ent-ps ent-logs eval-binary

# Default: build the web frontend, then the binary (frontend is embedded).
all: web build

# Build the web frontend into web/dist (required before the Go build embeds
# it; without it the embedded stub page is served instead). web/dist is
# gitignored; the .gitkeep keeps go:embed compilable on a fresh clone.
web:
	cd web && bun install && bun run build
	touch web/dist/.gitkeep

# Build the enterprise console SPAs and sync their dist into the go:embed
# directories (required before building the server roles to ship the real
# consoles; without it they serve a "console not built" placeholder). The
# spa dirs are gitignored — CI must run this target before go build/test.
ee-web:
	cd ee/configsvc-web && bun install && bun run build
	rm -rf internal/ee/configsvc/spa && mkdir -p internal/ee/configsvc/spa
	cp -R ee/configsvc-web/dist/ internal/ee/configsvc/spa/
	touch internal/ee/configsvc/spa/.gitkeep
	cd ee/opsconsole-web && bun install && bun run build
	rm -rf internal/ee/opsui/spa && mkdir -p internal/ee/opsui/spa
	cp -R ee/opsconsole-web/dist/ internal/ee/opsui/spa/
	touch internal/ee/opsui/spa/.gitkeep
	cd ee/skillsvc-web && bun install && bun run build
	rm -rf internal/ee/skillsvcui/spa && mkdir -p internal/ee/skillsvcui/spa
	cp -R ee/skillsvc-web/dist/ internal/ee/skillsvcui/spa/
	touch internal/ee/skillsvcui/spa/.gitkeep

build:
	go build -o build/woca ./cmd/woca

# The personal remote relay (design 2026-09-17-personal-relay-design): a
# standalone public-entry binary. Deliberately NOT in SERVER_ARTIFACTS
# (Makefile:271) — it is a personal-edition artifact and the role
# TUI/frontend gates do not apply to it.
build-relay:
	go build -o build/woca-relay ./cmd/woca-relay

# Personal edition: the single `woca` binary (CLI/TUI/REPL + serve) with all
# platform interfaces bound to their local implementations. This is the only
# artifact distributed to individual users; the microservice-role binaries
# (woca-gateway/-scheduler/-worker/...) live under cmd/ and are built by the
# build-server / dist-* targets below.
build-personal: web build build-relay

# Cross-compile the static linux/amd64 binary that the Harbor agent adapter
# (eval/woca_agent.py) uploads into benchmark task containers. CGO-free
# (modernc sqlite), so no cross toolchain is needed.
eval-binary:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o build/woca-linux-amd64 ./cmd/woca

test:
	go test ./...

race:
	go test -race ./...

web-test:
	cd web && bun install --frozen-lockfile && bun run test

ee-web-test:
	cd ee/configsvc-web && bun install --frozen-lockfile && bun run test
	cd ee/opsconsole-web && bun install --frozen-lockfile && bun run test
	cd ee/skillsvc-web && bun install --frozen-lockfile && bun run test

test-ui: web-test ee-web-test

# Browser E2E over the four frontend forms (e2e/README.md): the personal
# binary, the lab's real bridge/edge chain, the native-wizard shim, and —
# with WOCA_E2E_NATIVE_DEV=1 — the real wails process (opens a window).
#
# E2E_ENV keeps loopback off the proxy. Every target here talks only to the
# lab's fixed 127.0.0.1 ports, and Playwright's webServer readiness probe
# inherits the environment too: with HTTP_PROXY set (corporate machines, CI
# sandboxes) that probe otherwise reaches the proxy, gets a 502, and reports
# the port as "already used" when nothing is listening on it. e2e-install is
# deliberately NOT wrapped — downloading browsers needs the proxy.
E2E_ENV := env -u HTTP_PROXY -u HTTPS_PROXY -u http_proxy -u https_proxy \
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost

.PHONY: e2e-install e2e e2e-native-dev e2e-personal
# Dev-machine install. CI uses `--with-deps` instead (see .github/workflows),
# which additionally needs root to install the system libraries headless
# Chromium links against — pointless on a machine that already has them.
e2e-install:
	cd e2e && bun install --frozen-lockfile && npx playwright install chromium

# Every target depends on `web`: the Go binary embeds web/dist, so skipping
# the frontend build makes the browser tests drive the previous bundle.
e2e: web
	cd e2e && $(E2E_ENV) npx playwright test

e2e-native-dev: web
	cd e2e && WOCA_E2E_NATIVE_DEV=1 $(E2E_ENV) npx playwright test

# Just the personal form's specs, for iterating on one section. The webServers
# still all start (Playwright starts every webServer the config declares,
# whatever you select); what this saves is the other four forms' specs.
# Pass extra args with ARGS, e.g.
#   make e2e-personal ARGS=websearch.spec.ts
e2e-personal: web
	cd e2e && $(E2E_ENV) npx playwright test tests/personal $(ARGS)

vet:
	go vet ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .

# SSE event contract drift check (same gate as CI).
check-sse:
	ci/check-sse-contract.sh

# Matches CI's Go + UI jobs: format, vet, lint, unit tests, frontend tests,
# and the hygiene gates. CI additionally runs `race` and the Postgres-backed
# tests (service-gated; skipped here unless WOCA_TEST_PG_DSN/WOCA_PG_TEST_DSN
# are set), plus native-test on a macOS runner.
check: fmt vet lint test web-test ee-web-test check-sse check-deps check-server-artifacts check-editions

run:
	go run ./cmd/woca

# One-click: build and launch the interactive TUI.
tui: build
	./build/woca

# Build the frontend + binary and launch the embedded web UI.
serve: web build
	./build/woca serve

# Install the binary to /usr/local/bin.
install: build
	install -m 755 build/woca /usr/local/bin/woca

clean:
	rm -rf build

# Dependency hygiene (Phase 0 acceptance gate, design doc §5.1): the personal
# binary must never link pgx/NATS/Redis/OIDC-class dependencies. The baseline
# records every non-stdlib package reachable from ./cmd/woca; CI/local runs of
# check-deps fail when the set drifts (regenerate intentionally via
# deps-baseline and review the diff in the commit).
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

# Enterprise-only native client commands. These remain independent of the
# personal-edition default/build targets above.
native-test:
	cd native-client && go test ./...

native-build: web build-server
	# Bundle the bridge binary into the native client (embeddedbridge tag):
	# the app embeds it, installs to ~/.woca/native/bin on first run, and
	# spawns it as the zero-login local bridge (product design §5.1).
	cp build/woca-bridge native-client/bridgebin/woca-bridge
	cd native-client && wails build -clean -tags embeddedbridge

native-dev: build-server
	# Dev runs the SAME embedded bridge as production (product design
	# §5.1): bundle the freshly built woca-bridge and compile with the
	# embeddedbridge tag, so `wails dev` boots the zero-login local bridge
	# instead of needing WOCA_NATIVE_DEV_BFF_URL. Re-run to pick up bridge
	# source changes (the embedded copy is stamped at build time).
	cp build/woca-bridge native-client/bridgebin/woca-bridge
	cd native-client && wails dev -tags embeddedbridge

# Self-hosted enterprise stack: the persistence layer — PostgreSQL
# primary+replica and a 3-node NATS JetStream cluster, segmented per
# component class and reachable only through the single gateway on high
# ports. See deploy/selfhosted/README.md.
ENT_COMPOSE = docker compose -f deploy/selfhosted/docker-compose.yml

ent-up:
	$(ENT_COMPOSE) up -d

# compose down plus removing the inter-segment isolation rules from the
# Docker VM's DOCKER-USER chain (installed by the firewall service).
ent-down:
	$(ENT_COMPOSE) down
	docker run --rm --privileged --pid=host alpine:3.22 nsenter -t 1 -m -n -- \
		sh -c 'iptables -D DOCKER-USER -j WOCA-ENT-ISOLATION 2>/dev/null; iptables -F WOCA-ENT-ISOLATION 2>/dev/null; iptables -X WOCA-ENT-ISOLATION 2>/dev/null; echo woca-enterprise: isolation rules removed' 

# Full microservices deployment (design §5.3): every role + data plane +
# firewall isolation, with the observability stack (§12.3) attached.
ENT_FULL_COMPOSE = docker compose -f deploy/selfhosted/docker-compose.full.yml -f deploy/observability/docker-compose.observability.yml
ENT_OBS_SVCS = otel-collector jaeger prometheus alertmanager grafana

.PHONY: ent-full-up ent-full-down ent-obs-up ent-obs-down ent-obs-logs
ent-full-up:
	# up -d (not restart): the overlay changes app env/networks, and only
	# up recreates containers whose merged configuration drifted.
	$(ENT_FULL_COMPOSE) up -d
	# nginx resolves static upstream names once at config load; recreated
	# app containers get new IPs, so reload the gateway to re-resolve.
	$(ENT_FULL_COMPOSE) restart gateway
	@echo "full microservices stack up: business :8080, grafana :8085"

# Down removes the containers/networks AND the DOCKER-USER isolation rules
# installed by the firewall one-shot (same chain name as the base stack, so
# the base stack's ent-down cleanup covers it too).
ent-full-down:
	$(ENT_FULL_COMPOSE) down
	docker run --rm --privileged --pid=host alpine:3.22 nsenter -t 1 -m -n -- \
		sh -c 'iptables -D DOCKER-USER -j WOCA-ENT-ISOLATION 2>/dev/null; iptables -F WOCA-ENT-ISOLATION 2>/dev/null; iptables -X WOCA-ENT-ISOLATION 2>/dev/null; echo woca-enterprise: isolation rules removed'

# ─────────── ent-sandbox-demo: EE sandbox plane acceptance (spec §12) ───────────
# Drives the full stack's demo profile through the four-phase startup
# (design §11.2) and asserts all four acceptance levels; the sandbox-demo
# container's exit code is the verdict:
#
#   1. base plane: pg, nacos, nacos-init, woca-config, woca-scheduler,
#      executor-proxy, fakemodel (config comes up BEFORE the executor, so
#      its boot arms the backend's API key);
#   2. config plane: sandbox-init writes the model settings + the
#      contract-http backend (endpoint executor-proxy) and enables it;
#   3. execution plane: woca-executor (registers with nacos, declares the
#      sandbox capability);
#   4. acceptance: sandbox-demo (L1 scripted model → product + reclaim,
#      L2 cross-run continuity/fresh container/mutex, L3 hot options,
#      L4 isolation/403/egress/no leftovers).
#
# The stack is hermetic per run (down -v teardown) and needs no .env —
# the compose file's required variables get demo defaults here. The
# sandbox images (bash+python+tar base, marker variant) build from
# e2e/sandboxlab/demo. Requires a linux-container docker engine
# (OrbStack: hardening runs minimal, spec §11.3). ENT_SANDBOX_DEMO_KEEP=1
# keeps a failing stack up for troubleshooting.
ENT_DEMO_COMPOSE = docker compose -f deploy/selfhosted/docker-compose.full.yml --profile demo
ENT_DEMO_ENV = WOCA_PG_PASSWORD=woca-demo-pg \
	WOCA_PG_REPL_PASSWORD=woca-demo-repl \
	LADYM_PG_PASSWORD=woca-demo-ladym \
	LANGFUSE_PG_PASSWORD=woca-demo-lf-pg \
	LANGFUSE_CLICKHOUSE_PASSWORD=woca-demo-lf-ch \
	LANGFUSE_REDIS_AUTH=woca-demo-lf-redis \
	LANGFUSE_MINIO_ROOT_PASSWORD=woca-demo-lf-minio \
	LANGFUSE_SALT=woca-demo-lf-salt \
	LANGFUSE_ENCRYPTION_KEY=woca-demo-lf-enc \
	LANGFUSE_NEXTAUTH_SECRET=woca-demo-lf-na
ENT_DEMO_ROLES := woca-scheduler woca-config executor-proxy

.PHONY: ent-sandbox-demo
ent-sandbox-demo:
	@for r in $(ENT_DEMO_ROLES); do \
		GOOS=linux $(MAKE) --no-print-directory dist-role ROLE=$$r || exit 1; \
	done
	# The executor role package always carries the docker driver: without
	# -tags dockerlab the binary refuses to start (fail-closed no-backend).
	GOOS=linux $(MAKE) --no-print-directory dist-role ROLE=woca-executor || exit 1
	docker build -q -t woca-sandbox-base:demo \
		-f e2e/sandboxlab/demo/Dockerfile.sandbox-base e2e/sandboxlab/demo || exit 1
	docker build -q -t woca-sandbox-marker:demo \
		-f e2e/sandboxlab/demo/Dockerfile.sandbox-marker e2e/sandboxlab/demo || exit 1
	# Hermetic start: drop any previous project state (volumes included).
	-$(ENT_DEMO_ENV) $(ENT_DEMO_COMPOSE) down -v --remove-orphans --timeout 20
	# Phase 1 — base plane without the executor (config first: the
	# executor's boot resolves the enabled backend + its API key).
	$(ENT_DEMO_ENV) $(ENT_DEMO_COMPOSE) up -d --build \
		woca-pg-primary nacos nacos-init woca-config woca-scheduler-1 \
		executor-proxy fakemodel || exit 1
	# Phase 2 — config plane (model settings + backend, enabled).
	$(ENT_DEMO_ENV) $(ENT_DEMO_COMPOSE) run --rm sandbox-init || exit 1
	# Phase 3 — execution plane. Record the roles' boot state: the L3
	# hot-reload assertion requires NO restarts across the options change.
	$(ENT_DEMO_ENV) $(ENT_DEMO_COMPOSE) up -d --build woca-executor || exit 1
	@state0=$$(docker inspect -f '{{.Name}} {{.State.StartedAt}} restarts={{.RestartCount}}' \
		woca-ent-executor woca-ent-server-1 2>/dev/null); \
	rc=0; \
	$(ENT_DEMO_ENV) $(ENT_DEMO_COMPOSE) run --rm --build sandbox-demo || rc=$$?; \
	state1=$$(docker inspect -f '{{.Name}} {{.State.StartedAt}} restarts={{.RestartCount}}' \
		woca-ent-executor woca-ent-server-1 2>/dev/null); \
	if [ $$rc -eq 0 ] && [ "$$state0" != "$$state1" ]; then \
		echo "ent-sandbox-demo: executor/scheduler restarted during the demo (L3 requires no restarts)"; \
		echo "  before: $$state0"; echo "  after:  $$state1"; \
		rc=1; \
	fi; \
	if [ $$rc -eq 0 ]; then \
		if docker logs woca-ent-server-1 2>&1 | grep -q 'falling back to client-side archives'; then \
			echo "ent-sandbox-demo: scheduler logged a local archive fallback (no-local-fallback violated)"; \
			rc=1; \
		fi; \
	fi; \
	if [ $$rc -eq 0 ]; then echo "ent-sandbox-demo: ALL LEVELS GREEN"; \
	else echo "ent-sandbox-demo: FAILED (rc=$$rc)"; fi; \
	if [ "$$(docker ps -q --filter label=woca.sandbox=1 | wc -l | tr -d ' ')" != "0" ]; then \
		echo "ent-sandbox-demo: leftover sandbox containers:"; \
		docker ps --filter label=woca.sandbox=1; \
		docker ps -q --filter label=woca.sandbox=1 | xargs docker rm -f >/dev/null 2>&1 || true; \
	fi; \
	if [ "$$ENT_SANDBOX_DEMO_KEEP" = "1" ]; then \
		echo "ENT_SANDBOX_DEMO_KEEP=1 — stack left running for troubleshooting"; \
	else \
		$(ENT_DEMO_ENV) $(ENT_DEMO_COMPOSE) down -v --remove-orphans --timeout 20 >/dev/null 2>&1 || true; \
	fi; \
	exit $$rc

# Observability overlay (design §12.3): OTel Collector + Prometheus +
# Alertmanager + Grafana + Jaeger onto the full stack. Requires the full
# stack to be up (ent-full-up).

ent-obs-up:
	# up -d (not restart): the overlay changes app env/networks, and only
	# up recreates containers whose merged configuration drifted.
	$(ENT_FULL_COMPOSE) up -d gateway woca-scheduler-1 woca-scheduler-2 woca-worker-1 woca-worker-2 $(ENT_OBS_SVCS)
	# nginx resolves static upstream names once at config load; recreated
	# app containers get new IPs, so reload the gateway to re-resolve.
	$(ENT_FULL_COMPOSE) restart gateway
	@echo "observability up: grafana http://localhost:$${ENT_GRAFANA_PORT:-8085}"

ent-obs-down:
	$(ENT_FULL_COMPOSE) stop $(ENT_OBS_SVCS)

ent-obs-logs:
	$(ENT_FULL_COMPOSE) logs -f $(ENT_OBS_SVCS)

ent-ps:
	$(ENT_COMPOSE) ps

ent-logs:
	$(ENT_COMPOSE) logs -f

# Dev environment: the minimal twin of the full stack — one of every
# microservice role, a single PG and NATS node, no observability. Built
# for development 联调; data lives in woca-dev-* volumes (never touches
# full's). The full stack must be down first — host ports are shared by
# design, so the two are drop-in replacements.
DEV_COMPOSE = docker compose -f deploy/selfhosted/docker-compose.dev.yml

.PHONY: dev-up dev-down dev-ps dev-logs dev-bridge dev-langfuse
dev-up:
	# --build: dev runs the current source (the committed :local images
	# may predate the working tree).
	$(DEV_COMPOSE) up -d --build
	# nginx resolves upstream names once at config load; rebuilt app
	# containers get new IPs, so reload the gateway after --build.
	$(DEV_COMPOSE) restart gateway
	$(DEV_COMPOSE) ps
	@echo "dev stack up: gateway :8080 business, :9086 config console (single instances, no monitoring)"

# The dev bridge toolbox (profile: bridge): python/node/npm/bun container
# running woca-bridge — the browser drives :3490, tools execute inside.
dev-bridge:
	$(DEV_COMPOSE) --profile bridge up -d --build bridge
	@echo "dev bridge: http://localhost:$${DEV_BRIDGE_PORT:-3490} (enroll via DEV_ENROLL_TOKEN from the ops console, then sign in with an org key)"

# The dev Langfuse group (profile: langfuse): LLM tracing UI. The roles
# always push spans to the woca-collector bridge (mgmt segment, always
# on); this only brings up the Langfuse backend they end up in — no role
# reconfiguration involved.
dev-langfuse:
	$(DEV_COMPOSE) --profile langfuse up -d \
		langfuse-pg langfuse-clickhouse langfuse-redis langfuse-minio \
		langfuse-web langfuse-worker
	@echo "langfuse: http://localhost:$${DEV_LANGFUSE_PORT:-9300} (sign in $${LANGFUSE_INIT_USER_EMAIL:-admin@woca.local} / $${LANGFUSE_INIT_USER_PASSWORD:-woca-langfuse-admin} unless overridden in .env)"

dev-down:
	$(DEV_COMPOSE) --profile bridge --profile langfuse down

dev-ps:
	$(DEV_COMPOSE) ps

dev-logs:
	$(DEV_COMPOSE) logs -f

# ─────────── edge topology dev group (product design v1.5 §6.3) ───────────
# Control plane as ONE container group (edge + config + server + pg) with
# public/internal network layering standing in for the DMZ, plus the
# bridge as an independent group reaching in through the published edge
# port — scenario four's minimal twin.
EDGE_COMPOSE = docker compose -f deploy/selfhosted/docker-compose.edge.yml
EDGE_BRIDGE_COMPOSE = docker compose -f deploy/selfhosted/docker-compose.edge-bridge.yml

.PHONY: edge-up edge-down edge-bridge edge-bridge-down
edge-up:
	$(EDGE_COMPOSE) up -d --build
	$(EDGE_COMPOSE) ps
	@echo "edge group up: edge :8443 (public), tenant ops console 127.0.0.1:9091, config console 127.0.0.1:9086 (dev-only loopback publishes)"
	@echo "consoles: open by default; set WOCA_CONSOLE_USER/WOCA_CONSOLE_PASSWORD to require a username/password sign-in"

edge-down:
	$(EDGE_BRIDGE_COMPOSE) down || true
	$(EDGE_COMPOSE) down

edge-bridge:
	$(EDGE_BRIDGE_COMPOSE) up -d --build
	@echo "dev bridge: http://localhost:$${EDGE_BRIDGE_PORT:-3490} — set EDGE_ENROLL_TOKEN (ops console → bridge 注册) for first enrollment, then sign in with an org key (ops console → 租户与 key)"

edge-bridge-down:
	$(EDGE_BRIDGE_COMPOSE) down

# ─────────── netlab: 网络层 docker 隔离集成测试（e2e/netlab） ───────────
# The network layer split out of the e2e pyramid and tested on its own:
# each edition gets a dedicated compose topology whose docker networks
# stand in for the real zones (public front / simulated wan / private
# core), the services run as the FINAL distribution images (dist-role
# packages baked into build/dist/Dockerfile), and the Go driver asserts
# both the positive chain (frontend → relay/edge → tunnel → internal
# bridge) and the cross-zone isolation. Design:
# docs/superpowers/specs/2026-10-06-netlab-design.md.
#
#	make netlab-personal     个人版：client → woca-relay → 隧道 → 家庭 woca
#	make netlab-enterprise   企业版：client → woca-edge → 隧道 → bridge + core
#	make netlab              both, independently reported
#
# NETLAB_KEEP=1 keeps a failing stack up for manual 排查 (see
# e2e/netlab/README.md). Requires a linux-container docker engine; the
# role packages cross-compile with GOOS=linux at the host's GOARCH.
NETLAB_PERSONAL_ROLES := woca woca-relay
NETLAB_ENTERPRISE_ROLES := woca-scheduler woca-config woca-edge woca-bridge

.PHONY: netlab netlab-personal netlab-enterprise
netlab: netlab-personal netlab-enterprise

netlab-personal:
	@for r in $(NETLAB_PERSONAL_ROLES); do \
		GOOS=linux $(MAKE) --no-print-directory dist-role ROLE=$$r || exit 1; \
	done
	go test -tags netlab -count=1 -timeout 25m -run 'TestPersonalRelayChain' -v ./e2e/netlab

netlab-enterprise:
	@for r in $(NETLAB_ENTERPRISE_ROLES); do \
		GOOS=linux $(MAKE) --no-print-directory dist-role ROLE=$$r || exit 1; \
	done
	go test -tags netlab -count=1 -timeout 40m -run 'TestEnterpriseEdgeChain' -v ./e2e/netlab

# ─────────── sandboxlab: 沙箱执行链 e2e（e2e/sandboxlab） ───────────
# The off-host execution chain on real role containers (plan task 27,
# design 2026-10-19-sandbox-console-design.md §6): configsvc registers +
# verifies + enables a contract-http backend (the fakesandbox container),
# a remote tenant creates a run, and the worker executes it inside the
# sandbox and downloads the products back to the shared workspace.
#
#	make sandboxlab    full chain (403 gate → verify → enable → run → artifact)
#
# SANDBOXLAB_KEEP=1 keeps a failing stack up. Requires a linux-container
# docker engine; the role packages cross-compile with GOOS=linux at the
# host's GOARCH. The scripted anthropic-compatible model (fakemodel/)
# needs no package — it builds in its own docker stage.
SANDBOXLAB_ROLES := woca-scheduler woca-config woca-worker woca-fakesandbox

.PHONY: sandboxlab
sandboxlab:
	@for r in $(SANDBOXLAB_ROLES); do \
		GOOS=linux $(MAKE) --no-print-directory dist-role ROLE=$$r || exit 1; \
	done
	go test -tags sandboxlab -count=1 -timeout 25m -run 'TestSandboxLab' -v ./e2e/sandboxlab

# ─────────── executor-dockerlab: docker 驱动冒烟（internal/ee/executor） ──────
# The EE sandbox plane's real Docker driver (docker_backend.go) lives behind
# the dockerlab build tag, so plain `go test ./...` needs no daemon. This
# target runs its smoke test against the local docker engine: create a
# hardened alpine container → exec variants → stdin → timeout kill ladder →
# CopyIn/CopyOut round-trip → Start/Kill → remove. DOCKER_HOST is honored
# for OrbStack-style sockets; without a reachable daemon the test skips.
.PHONY: executor-dockerlab-test
executor-dockerlab-test:
	go test -tags dockerlab -count=1 -timeout 10m -run 'DockerLab' -v ./internal/ee/executor

# ─────────── complab: component integration tests (e2e/complab) ───────────
# Unlike netlab / sandboxlab there is no docker topology here: woca is
# assembled in-process the way production wires it, each component is checked
# for actually working, and components are exercised in combination
# (search -> fetch).
#
#	make complab          every component (including WebSearch / WebFetch,
#	                      which need internet)
#	make complab-offline  only the components that work offline (CI with no
#	                      egress)
#
# The failure semantics for the networked components are deliberate: no
# internet -> skip (environment problem); internet present but every free
# backend blocked -> FAIL (a real finding: this network needs a self-hosted
# SearXNG or Tavily keyless) — see the Web search section in the settings UI.
.PHONY: complab complab-offline
complab:
	go test -tags complab -count=1 -timeout 10m -v ./e2e/complab

complab-offline:
	WOCA_COMPLAB_OFFLINE=1 go test -tags complab -count=1 -timeout 5m -v ./e2e/complab

# Server-side artifacts (design §5.1/Phase 3 T9): no TUI/REPL/CLI surfaces.
# woca-edge joins the set with the v1.5 topology split (stateless public
# entry; still a role artifact — thin, no interactive surfaces). The EE
# sandbox plane adds woca-executor + executor-proxy (sandbox-plane task 17).
SERVER_ARTIFACTS := woca-scheduler woca-tenant woca-worker woca-config woca-bridge woca-edge woca-ops woca-executor executor-proxy

.PHONY: build-server check-server-artifacts check-editions
build-server:
	@for a in $(SERVER_ARTIFACTS); do \
		go build -o build/$$a ./cmd/$$a || exit 1; \
	done
	@echo "server artifacts: $(SERVER_ARTIFACTS)"

# Per-role distribution packages (deploy/micro): `make woca-ops` bakes the
# role name into start/stop scripts, stages the binary beside them, and
# tars one self-contained bundle — deliver it into a container (COPY or
# volume), extract, and operators run start.sh / start.ps1 with no
# arguments. Cross-compile for containers by overriding the toolchain env
# (the build is CGO-free):
#   GOOS=linux GOARCH=amd64 make woca-worker
# `make dist-micro` produces every role's package. `make dist-linux`
# produces the linux set consumed by the compose stacks and
# `make dist-images` (docker on the same host); override GOARCH for
# foreign servers, e.g. GOARCH=amd64 make dist-images.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
DIST_GOOS ?= $(shell go env GOOS)
DIST_GOARCH ?= $(shell go env GOARCH)
DIST_EXE := $(if $(filter windows,$(DIST_GOOS)),.exe,)
.PHONY: $(SERVER_ARTIFACTS) dist-micro dist-role dist-linux dist-images
$(SERVER_ARTIFACTS):
	@$(MAKE) --no-print-directory dist-role ROLE=$@

dist-micro:
	@for r in $(SERVER_ARTIFACTS); do \
		$(MAKE) --no-print-directory dist-role ROLE=$$r || exit 1; \
	done

dist-linux:
	@for r in $(SERVER_ARTIFACTS); do \
		GOOS=linux GOARCH=$(DIST_GOARCH) $(MAKE) --no-print-directory dist-role ROLE=$$r || exit 1; \
	done

# Bake every role package into a local image (woca-<role>:local) through
# the shared deploy/micro/Dockerfile. Tag and push for the Terraform/ECR
# flow: see deploy/terraform/aws/README.md.
dist-images: dist-linux
	@for r in $(SERVER_ARTIFACTS); do \
		docker build -q -t $$r:local \
			-f build/dist/Dockerfile --build-arg ROLE_PKG=$$r.tar.gz \
			build/dist || exit 1; \
		echo "image: $$r:local"; \
	done

# ROLE_BUILD_TAGS: per-role default build tags for dist packages. The
# executor's docker driver lives behind -tags dockerlab (plain builds
# refuse to start — fail-closed no-backend), and a shipped executor
# package without it would be dead weight; override with GO_BUILDTAGS.
dist-role:
	@test -n "$(ROLE)" || { echo "usage: make dist-role ROLE=woca-ops (or just: make woca-ops)"; exit 2; }
	@test -d cmd/$(ROLE) || { echo "unknown role $(ROLE): no cmd/$(ROLE)"; exit 2; }
	@tags="$(GO_BUILDTAGS)"; \
	if [ -z "$$tags" ] && [ "$(ROLE)" = "woca-executor" ]; then tags=dockerlab; fi; \
	if [ -n "$$tags" ]; then tagflags="-tags $$tags"; else tagflags=""; fi; \
	stage="build/dist/.stage-$(ROLE)"; out="build/dist/$(ROLE)-$(VERSION)-$(DIST_GOOS)-$(DIST_GOARCH)"; \
	rm -rf "$$stage" "$$out" "$$out.tar.gz"; \
	mkdir -p "$$stage/$(ROLE)"; \
	CGO_ENABLED=0 GOOS=$(DIST_GOOS) GOARCH=$(DIST_GOARCH) \
		go build $$tagflags -trimpath -ldflags "-s -w" -o "$$stage/$(ROLE)/$(ROLE)$(DIST_EXE)" ./cmd/$(ROLE) || exit 1; \
	if [ "$(DIST_GOOS)" = "windows" ]; then scripts="start.ps1 stop.ps1"; else scripts="start.sh stop.sh"; fi; \
	for f in $$scripts; do \
		sed -e "s/@ROLE@/$(ROLE)/g" deploy/micro/$$f.tmpl > "$$stage/$(ROLE)/$$f" || exit 1; \
	done; \
	if [ "$(DIST_GOOS)" != "windows" ]; then chmod +x "$$stage/$(ROLE)/start.sh" "$$stage/$(ROLE)/stop.sh"; fi; \
	printf '%s\n' "$(VERSION)" > "$$stage/$(ROLE)/VERSION"; \
	cp deploy/micro/README.md "$$stage/$(ROLE)/"; \
	tar -czf "$$out.tar.gz" -C "$$stage" "$(ROLE)"; \
	shasum -a 256 "$$out.tar.gz" | tee "$$out.tar.gz.sha256"; \
	rm -rf "$$stage"; \
	cp "$$out.tar.gz" "build/dist/$(ROLE).tar.gz"; \
	cp deploy/micro/Dockerfile build/dist/Dockerfile; \
	echo "dist: $$out.tar.gz (alias: build/dist/$(ROLE).tar.gz)"

# Dependency hygiene: server artifacts must not link the interactive
# surfaces (TUI, REPL) or the one-shot CLI's root command wiring.
check-server-artifacts: build-server
	@for a in $(SERVER_ARTIFACTS); do \
		if go list -deps ./cmd/$$a | grep -qE 'internal/(tui|repl)'; then \
			echo "$$a links interactive surfaces"; exit 1; \
		fi; \
	done
	@for p in woca/web internal/ee/opsui internal/ee/configsvc internal/ee/skillsvcui; do \
		if go list -deps ./cmd/woca-scheduler | grep -q "projanvil/woca/$$p$$"; then \
			echo "woca-scheduler links a web/frontend package ($$p): it must stay pageless"; exit 1; \
		fi; \
	done
	@echo "server artifacts pass dependency hygiene"

# Edition boundary hygiene (open-core discipline): exactly two editions —
# personal (`woca`) and enterprise (the server roles under internal/ee).
# The personal binary never links the enterprise tree; the enterprise tree
# (and every server artifact) never links the interactive/personal
# frontends; and no core package (outside internal/ee and the server-role
# entry points) imports internal/ee at all — enterprise capabilities reach
# the core only through the seams (server.MsgPlane, server.NativeAuth,
# servefront.Edition, wocadb.DSNResolver).
check-editions: build build-server
	@if go list -deps ./cmd/woca | grep -q 'woca/internal/ee/'; then \
		echo "personal binary links the enterprise tree (internal/ee)"; exit 1; \
	fi
	@if go list -deps ./internal/ee/... | grep -qE 'internal/(tui|repl|console|notify|oauth|sdkout)'; then \
		echo "enterprise tree links personal/interactive packages"; exit 1; \
	fi
	@out=$$(go list -f '{{.ImportPath}} {{join .Imports " "}} {{join .TestImports " "}}' ./internal/... ./web/... ./cmd/woca ./cmd/woca-relay 2>/dev/null \
		| grep 'woca/internal/ee/' | grep -v '^github.com/projanvil/woca/internal/ee/'); \
	if [ -n "$$out" ]; then \
		echo "core code imports internal/ee (enterprise belongs behind the seams):"; \
		echo "$$out"; exit 1; \
	fi
	@echo "edition boundaries pass dependency hygiene"

# Personal relay end-to-end (design §10): key create → connect → login →
# tunneled API + SSE → revoke → both sides error. Loopback, throwaway
# state; no model provider required (the run surface is exercised through
# the tunneled API and the global events SSE).
e2e-relay:
	ci/e2e-relay.sh
