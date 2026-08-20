.PHONY: all build build-personal test vet fmt run clean lint race check tui serve web install deps-baseline check-deps native-test native-build native-dev ent-up ent-down ent-ps ent-logs

# Default: build the web frontend, then the binary (frontend is embedded).
all: web build

# Build the web frontend into web/dist (required before the Go build embeds it).
web:
	cd web && npm install && npm run build

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

ent-ps:
	$(ENT_COMPOSE) ps

ent-logs:
	$(ENT_COMPOSE) logs -f
