package main

// The production backend seam and its build matrix. The docker driver
// lives behind the dockerlab build tag (Task 7's constraint: plain
// `go build ./...` must compile zero docker dependencies), so this
// package cannot reference executor.NewDockerBackend directly. Instead:
//
//   - dockerlab builds: backend_dockerlab.go's init installs the factory
//     below, and main provisions real containers.
//   - plain builds: the factory stays nil, and main refuses to start
//     unless WOCA_DEV_FAKE_BACKEND=1 explicitly opts into the in-process
//     fake (development only — a loud WARN, never a silent downgrade).
//
// There is deliberately no !dockerlab file: the zero value of this
// variable IS the plain-build behavior.

import "github.com/projanvil/woca/internal/ee/executor"

// backendFactory builds the production container backend (host is the
// docker daemon address; "" means the driver's default socket). Nil in
// plain builds — see above.
var backendFactory func(host string) (executor.Backend, error)
