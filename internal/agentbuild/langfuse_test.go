package agentbuild

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tracing"
)

// langfuseCapture records the auth/ingestion headers of one export.
type langfuseCapture struct {
	auth string
	ver  string
	body []byte
	seen chan struct{}
}

func newLangfuseCapture(t *testing.T) (*langfuseCapture, *httptest.Server) {
	t.Helper()
	c := &langfuseCapture{seen: make(chan struct{}, 4)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.auth = r.Header.Get("Authorization")
		c.ver = r.Header.Get("x-langfuse-ingestion-version")
		c.body = body
		w.WriteHeader(http.StatusOK)
		c.seen <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	return c, srv
}

func newSettingsStore(t *testing.T) *settings.Store {
	t.Helper()
	st, err := settings.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func recordOneSpan(t *testing.T, runID string) {
	t.Helper()
	b := tracing.Start(t.Context(), "chat", tracing.KindLLM)
	b.Set(tracing.AttrRequestModel, "claude-sonnet-4-5")
	b.End(tracing.WithRun(t.Context(), runID, "sess"), nil)
}

// resetLangfuseInstall clears the once-per-process guard between tests.
func resetLangfuseInstall(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		langfuseInstallMu.Lock()
		langfuseInstalled = false
		langfuseInstallMu.Unlock()
	})
	langfuseInstallMu.Lock()
	langfuseInstalled = false
	langfuseInstallMu.Unlock()
}

func TestInstallLangfuseSinkFromSettings(t *testing.T) {
	resetLangfuseInstall(t)
	tracing.SetDefault(nil)
	t.Cleanup(func() { tracing.SetDefault(nil) })

	c, srv := newLangfuseCapture(t)
	st := newSettingsStore(t)
	cfg := st.Load()
	cfg.LangfuseEnabled = true
	cfg.LangfuseBaseURL = srv.URL
	cfg.LangfusePublicKey = "pk-test"
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSecret(settings.SecretLangfuseSecretKey, "sk-test"); err != nil {
		t.Fatal(err)
	}

	closeFn := installLangfuseSink(st, testLogger())
	if closeFn == nil {
		t.Fatal("sink not installed from settings")
	}
	defer closeFn()

	recordOneSpan(t, "run-1")
	waitFor(t, c.seen)
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("pk-test:sk-test")); c.auth != want {
		t.Fatalf("Authorization = %q", c.auth)
	}
	if c.ver != "4" {
		t.Fatalf("ingestion header = %q", c.ver)
	}
}

func TestInstallLangfuseSinkEnvFallback(t *testing.T) {
	resetLangfuseInstall(t)
	tracing.SetDefault(nil)
	t.Cleanup(func() { tracing.SetDefault(nil) })

	c, srv := newLangfuseCapture(t)
	t.Setenv("WOCA_LANGFUSE_HOST", srv.URL)
	t.Setenv("WOCA_LANGFUSE_PUBLIC_KEY", "pk-env")
	t.Setenv("WOCA_LANGFUSE_SECRET_KEY", "sk-env")

	st := newSettingsStore(t) // unconfigured: env is the fallback
	closeFn := installLangfuseSink(st, testLogger())
	if closeFn == nil {
		t.Fatal("sink not installed from env")
	}
	defer closeFn()

	recordOneSpan(t, "run-env")
	waitFor(t, c.seen)
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("pk-env:sk-env")); c.auth != want {
		t.Fatalf("Authorization = %q", c.auth)
	}
}

// TestInstallLangfuseSinkEnterpriseOTelWins: when the enterprise OTel
// exporter owns export (its endpoint is configured), the plugin stays off —
// double export would double-count.
func TestInstallLangfuseSinkEnterpriseOTelWins(t *testing.T) {
	t.Setenv("WOCA_OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	st := newSettingsStore(t)
	cfg := st.Load()
	cfg.LangfuseEnabled = true
	cfg.LangfuseBaseURL = "https://langfuse.example"
	cfg.LangfusePublicKey = "pk"
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if closeFn := installLangfuseSink(st, testLogger()); closeFn != nil {
		t.Fatal("plugin must stay off when the enterprise OTel endpoint is set")
	}
}

// TestInstallLangfuseSinkIncompleteDegrades: enabled but missing the secret
// degrades to off — observability never takes the agent down.
func TestInstallLangfuseSinkIncompleteDegrades(t *testing.T) {
	resetLangfuseInstall(t)
	tracing.SetDefault(nil)
	t.Cleanup(func() { tracing.SetDefault(nil) })
	st := newSettingsStore(t)
	cfg := st.Load()
	cfg.LangfuseEnabled = true
	cfg.LangfuseBaseURL = "https://langfuse.example"
	cfg.LangfusePublicKey = "pk"
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if closeFn := installLangfuseSink(st, testLogger()); closeFn != nil {
		t.Fatal("incomplete config must degrade to off")
	}
}

// TestInstallLangfuseSinkExplicitOffBeatsEnv: a persisted
// langfuse_enabled=false row is authoritative — the environment must not
// re-enable what settings turned off.
func TestInstallLangfuseSinkExplicitOffBeatsEnv(t *testing.T) {
	resetLangfuseInstall(t)
	t.Setenv("WOCA_LANGFUSE_HOST", "https://lf.example")
	t.Setenv("WOCA_LANGFUSE_PUBLIC_KEY", "pk-env")
	t.Setenv("WOCA_LANGFUSE_SECRET_KEY", "sk-env")

	st := newSettingsStore(t)
	cfg := st.Load()
	cfg.LangfuseEnabled = false // persisted explicitly (Save writes false too)
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if closeFn := installLangfuseSink(st, testLogger()); closeFn != nil {
		t.Fatal("explicit settings off must beat the environment")
	}
}

// TestInstallLangfuseSinkOncePerProcess: a second BuildAgent (every sdk
// session runs one) must not nest another chain — spans would export twice.
func TestInstallLangfuseSinkOncePerProcess(t *testing.T) {
	resetLangfuseInstall(t)
	st := newSettingsStore(t)
	cfg := st.Load()
	cfg.LangfuseEnabled = true
	cfg.LangfuseBaseURL = "https://lf.example"
	cfg.LangfusePublicKey = "pk"
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSecret(settings.SecretLangfuseSecretKey, "sk"); err != nil {
		t.Fatal(err)
	}
	if closeFn := installLangfuseSink(st, testLogger()); closeFn == nil {
		t.Fatal("first install failed")
	}
	if closeFn := installLangfuseSink(st, testLogger()); closeFn != nil {
		t.Fatal("second install must be skipped (once per process)")
	}
}

func TestInstallLangfuseSinkDisabled(t *testing.T) {
	if closeFn := installLangfuseSink(newSettingsStore(t), testLogger()); closeFn != nil {
		t.Fatal("unconfigured plugin must not install")
	}
}

func waitFor(t *testing.T, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no export arrived")
	}
}
