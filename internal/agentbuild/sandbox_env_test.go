package agentbuild

import "testing"

func TestSandboxFromEnv(t *testing.T) {
	t.Setenv("WOCA_SANDBOX_ENDPOINT", "")
	if cfg := SandboxFromEnv(); cfg != nil {
		t.Fatalf("no endpoint = %+v, want nil (local executor)", cfg)
	}
	t.Setenv("WOCA_SANDBOX_ENDPOINT", "http://sbx:8080")
	t.Setenv("WOCA_SANDBOX_API_KEY", "k")
	t.Setenv("WOCA_SANDBOX_TIMEOUT_MS", "1500")
	cfg := SandboxFromEnv()
	if cfg == nil || cfg.Endpoint != "http://sbx:8080" || cfg.APIKey != "k" || cfg.Timeout.Milliseconds() != 1500 {
		t.Fatalf("cfg = %+v", cfg)
	}
}
