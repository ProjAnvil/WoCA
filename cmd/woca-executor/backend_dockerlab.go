//go:build dockerlab

package main

// The dockerlab half of the backend seam (see backend_factory.go): this
// file is the only place cmd/woca-executor touches the docker driver, so
// plain builds never import it.

import "github.com/projanvil/woca/internal/ee/executor"

func init() {
	backendFactory = func(host string) (executor.Backend, error) {
		return executor.NewDockerBackend(host)
	}
}
