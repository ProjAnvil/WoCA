//go:build !dev

package main

import "testing"

func TestProductionBuildIgnoresDevelopmentServerEnvironment(t *testing.T) {
	t.Setenv("WOCA_NATIVE_DEV_BFF_URL", "http://localhost:9090")
	if got := developmentServerURL(); got != "" {
		t.Fatalf("developmentServerURL() = %q, want empty production override", got)
	}
}
