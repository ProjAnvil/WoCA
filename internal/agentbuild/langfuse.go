package agentbuild

import (
	"os"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tracing"
	"github.com/projanvil/woca/internal/tracing/plugins/langfuse"
)

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
	if os.Getenv("WOCA_OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
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
	if !cfg.LangfuseEnabled && os.Getenv("WOCA_LANGFUSE_HOST") == "" {
		return nil
	}
	if host == "" || publicKey == "" || secretKey == "" {
		log.Warn().Msg("langfuse plugin enabled but configuration incomplete (base URL / public key / secret key); export disabled")
		return nil
	}
	sink, err := langfuse.New(langfuse.Config{Host: host, PublicKey: publicKey, SecretKey: secretKey})
	if err != nil {
		log.Warn().Err(err).Msg("langfuse plugin disabled")
		return nil
	}
	tracing.SetDefault(tracing.NewMultiSink(tracing.Default(), sink))
	return func() { _ = sink.Close() }
}
