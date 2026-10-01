package agentbuild

import (
	"os"
	"sync"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tracing"
	"github.com/projanvil/woca/internal/tracing/plugins/langfuse"
)

// langfuseInstallMu guards the once-per-process install: the sink chains
// onto the process-wide tracing default, so a second BuildAgent (every sdk
// session runs one) must not nest another chain — every span would export
// twice. Configuration is read from the process's settings store; changes
// apply on restart.
var langfuseInstallMu sync.Mutex
var langfuseInstalled bool

// otelExportOwnsExport reports whether the OTel exporter owns span export
// (its endpoint env is set — either spelling obs.FromEnv accepts): the
// plugin stays off then, or both paths would double-count every span.
func otelExportOwnsExport() bool {
	return os.Getenv("WOCA_OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
}

// installLangfuseSink resolves the Langfuse observability plugin from the
// settings store (langfuse-* keys) with WOCA_LANGFUSE_* environment
// fallback, and chains the sink onto the process-wide tracing default.
// It returns the sink's teardown (flush) or nil when the plugin stays off.
//
// Rules:
//   - the enterprise OTel exporter owns export when its endpoint is set
//     (WOCA_OTEL_EXPORTER_OTLP_ENDPOINT): the plugin stays off there, or
//     both paths would double-count every span;
//   - settings win; the environment is the fallback for forms without a
//     settings surface (embedded hosts, containers);
//   - an enabled-but-incomplete configuration degrades to off with a
//     warning — observability never takes the agent down.
//
// The sink installs for every form that runs through BuildAgent (CLI, TUI,
// serve, sdk); enterprise deployments receive the keys from the config
// plane instead of user writes (the settings API rejects them there).
func installLangfuseSink(st *settings.Store, log zerolog.Logger) func() {
	if otelExportOwnsExport() {
		return nil
	}
	var cfg settings.Settings
	secretKey := ""
	if st != nil {
		cfg = st.Load()
		secretKey = st.GetSecret(settings.SecretLangfuseSecretKey)
	}
	host, publicKey := cfg.LangfuseBaseURL, cfg.LangfusePublicKey
	if host == "" {
		host = os.Getenv("WOCA_LANGFUSE_HOST")
	}
	if publicKey == "" {
		publicKey = os.Getenv("WOCA_LANGFUSE_PUBLIC_KEY")
	}
	if secretKey == "" {
		secretKey = os.Getenv("WOCA_LANGFUSE_SECRET_KEY")
	}
	// Master switch: an explicit settings off beats the environment (the
	// persisted langfuse_enabled row is authoritative); the environment
	// implies enabled only where settings never recorded a stance.
	explicitOff := st != nil && st.HasKey("langfuse_enabled") && !cfg.LangfuseEnabled
	if (!cfg.LangfuseEnabled && os.Getenv("WOCA_LANGFUSE_HOST") == "") || explicitOff {
		return nil
	}
	if host == "" || publicKey == "" || secretKey == "" {
		log.Warn().Msg("langfuse plugin enabled but configuration incomplete (base URL / public key / secret key); export disabled")
		return nil
	}
	langfuseInstallMu.Lock()
	defer langfuseInstallMu.Unlock()
	if langfuseInstalled {
		return nil
	}
	sink, err := langfuse.New(langfuse.Config{Host: host, PublicKey: publicKey, SecretKey: secretKey})
	if err != nil {
		log.Warn().Err(err).Msg("langfuse plugin disabled")
		return nil
	}
	langfuseInstalled = true
	tracing.SetDefault(tracing.NewMultiSink(tracing.Default(), sink))
	return func() { _ = sink.Close() }
}
