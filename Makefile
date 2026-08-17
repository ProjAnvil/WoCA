.PHONY: all build test vet fmt run clean lint race check tui serve web install

# Default: build the web frontend, then the binary (frontend is embedded).
all: web build

# Build the web frontend into web/dist (required before the Go build embeds it).
web:
	cd web && npm install && npm run build

build:
	go build -o build/woca ./cmd/woca

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
