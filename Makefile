.PHONY: all build build-personal test vet fmt run clean lint race check tui serve web ee-web install deps-baseline check-deps native-test native-build native-dev ent-up ent-down ent-ps ent-logs

# Default: build the web frontend, then the binary (frontend is embedded).
all: web build

# Build the web frontend into web/dist (required before the Go build embeds it).
web:
	cd web && bun install && bun run build

# Build the enterprise console SPAs and sync their dist into the go:embed
# directories (required before building the server roles — the embedded
# consoles are committed, so this must run whenever ee/*-web changes).
ee-web:
	cd ee/configsvc-web && bun install && bun run build
	rm -rf internal/ee/configsvc/spa && mkdir -p internal/ee/configsvc/spa
	cp -R ee/configsvc-web/dist/ internal/ee/configsvc/spa/
	cd ee/opsconsole-web && bun install && bun run build
	rm -rf internal/ee/opsui/spa && mkdir -p internal/ee/opsui/spa
	cp -R ee/opsconsole-web/dist/ internal/ee/opsui/spa/
	cd ee/skillsvc-web && bun install && bun run build
	rm -rf internal/ee/skillsvc/spa && mkdir -p internal/ee/skillsvc/spa
	cp -R ee/skillsvc-web/dist/ internal/ee/skillsvc/spa/

build:
	go build -o build/woca ./cmd/woca

# Personal edition: the single `woca` binary (CLI/TUI/REPL + serve) with all
# platform interfaces bound to their local implementations. This is the only
# artifact distributed to individual users; microservice-role binaries
# (woca-gateway/-scheduler/-worker/...) get their own cmd/ entries and
# targets in Phase 3 (see the microservices design doc §5.1).
build-personal: web build

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .

check: fmt vet test

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
	go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./cmd/woca | sort -u > ci/deps-personal.txt

check-deps: build
	@mkdir -p build
	@go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./cmd/woca | sort -u > build/deps-personal.actual
	@diff -u ci/deps-personal.txt build/deps-personal.actual && echo "personal-edition deps match baseline"

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

.PHONY: ent-full-up ent-full-down
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

# Observability overlay (design §12.3): OTel Collector + Prometheus +
# Alertmanager + Grafana + Jaeger onto the full stack. Requires the full
# stack to be up (ent-full-up).

ent-obs-up:
	# up -d (not restart): the overlay changes app env/networks, and only
	# up recreates containers whose merged configuration drifted.
	$(ENT_FULL_COMPOSE) up -d gateway woca-server-1 woca-server-2 woca-worker-1 woca-worker-2 $(ENT_OBS_SVCS)
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

.PHONY: dev-up dev-down dev-ps dev-logs
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

dev-down:
	$(DEV_COMPOSE) --profile bridge down

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

# Server-side artifacts (design §5.1/Phase 3 T9): no TUI/REPL/CLI surfaces.
# woca-edge joins the set with the v1.5 topology split (stateless public
# entry; still a role artifact — thin, no interactive surfaces).
SERVER_ARTIFACTS := woca-server woca-tenant woca-worker woca-config woca-bridge woca-edge woca-ops

.PHONY: build-server check-server-artifacts check-editions
build-server:
	@for a in $(SERVER_ARTIFACTS); do \
		go build -o build/$$a ./cmd/$$a || exit 1; \
	done
	@echo "server artifacts: $(SERVER_ARTIFACTS)"

# Dependency hygiene: server artifacts must not link the interactive
# surfaces (TUI, REPL) or the one-shot CLI's root command wiring.
check-server-artifacts: build-server
	@for a in $(SERVER_ARTIFACTS); do \
		if go list -deps ./cmd/$$a | grep -qE 'internal/(tui|repl)'; then \
			echo "$$a links interactive surfaces"; exit 1; \
		fi; \
	done
	@for p in woca/web internal/ee/opsui internal/ee/configsvc internal/ee/skillsvc; do \
		if go list -deps ./cmd/woca-server | grep -q "projanvil/woca/$$p$$"; then \
			echo "woca-server links a web/frontend package ($$p): it must stay pageless"; exit 1; \
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
.PHONY: check-editions
check-editions: build build-server
	@if go list -deps ./cmd/woca | grep -q 'woca/internal/ee/'; then \
		echo "personal binary links the enterprise tree (internal/ee)"; exit 1; \
	fi
	@if go list -deps ./internal/ee/... | grep -qE 'internal/(tui|repl|console|notify|oauth|sdkout)'; then \
		echo "enterprise tree links personal/interactive packages"; exit 1; \
	fi
	@out=$$(go list -f '{{.ImportPath}} {{join .Imports " "}} {{join .TestImports " "}}' ./internal/... ./web/... ./cmd/woca 2>/dev/null \
		| grep 'woca/internal/ee/' | grep -v '^github.com/projanvil/woca/internal/ee/'); \
	if [ -n "$$out" ]; then \
		echo "core code imports internal/ee (enterprise belongs behind the seams):"; \
		echo "$$out"; exit 1; \
	fi
	@echo "edition boundaries pass dependency hygiene"
