.PHONY: all build build-personal test vet fmt run clean lint race check tui serve web install deps-baseline check-deps native-test native-build native-dev ent-up ent-down ent-ps ent-logs

# Default: build the web frontend, then the binary (frontend is embedded).
all: web build

# Build the web frontend into web/dist (required before the Go build embeds it).
web:
	cd web && bun install && bun run build

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

native-build: web
	cd native-client && wails build -clean

native-dev:
	cd native-client && wails dev

# woca-enterprise: the microservice (enterprise) edition persistence layer —
# PostgreSQL primary+replica and a 3-node NATS JetStream cluster, segmented
# per component class and reachable only through the single gateway on high
# ports. See deploy/enterprise/README.md.
ENT_COMPOSE = docker compose -f deploy/enterprise/docker-compose.yml

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
ENT_FULL_COMPOSE = docker compose -f deploy/enterprise/docker-compose.full.yml -f deploy/observability/docker-compose.observability.yml
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

# Server-side artifacts (design §5.1/Phase 3 T9): no TUI/REPL/CLI surfaces.
SERVER_ARTIFACTS := woca-server woca-tenant woca-worker

.PHONY: build-server check-server-artifacts
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
	@echo "server artifacts pass dependency hygiene"
