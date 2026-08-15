// Package logging provides a thin wrapper over zerolog for the woca harness:
// level parsing and a configured logger constructor.
package logging

import (
	"fmt"
	"io"
	"strings"

	"github.com/rs/zerolog"
)

// ParseLevel converts a textual level into a zerolog.Level. The empty string
// means disabled. Unknown values are an error.
func ParseLevel(s string) (zerolog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return zerolog.Disabled, nil
	case "trace":
		return zerolog.TraceLevel, nil
	case "debug":
		return zerolog.DebugLevel, nil
	case "info":
		return zerolog.InfoLevel, nil
	case "warn", "warning":
		return zerolog.WarnLevel, nil
	case "error":
		return zerolog.ErrorLevel, nil
	case "fatal":
		return zerolog.FatalLevel, nil
	case "panic":
		return zerolog.PanicLevel, nil
	case "disabled", "off", "none":
		return zerolog.Disabled, nil
	default:
		return zerolog.Disabled, fmt.Errorf("logging: unknown level %q", s)
	}
}

// New builds a timestamped logger at the given level writing to out.
func New(out io.Writer, level zerolog.Level) zerolog.Logger {
	return zerolog.New(out).Level(level).With().Timestamp().Logger()
}

// NewConsole builds a logger writing human-readable (console) output to out.
func NewConsole(out io.Writer, level zerolog.Level) zerolog.Logger {
	return zerolog.New(zerolog.ConsoleWriter{Out: out, NoColor: true}).
		Level(level).With().Timestamp().Logger()
}
