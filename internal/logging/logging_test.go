package logging

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want zerolog.Level
	}{
		{"", zerolog.Disabled},
		{"debug", zerolog.DebugLevel},
		{"DEBUG", zerolog.DebugLevel},
		{"info", zerolog.InfoLevel},
		{"warn", zerolog.WarnLevel},
		{"warning", zerolog.WarnLevel},
		{"error", zerolog.ErrorLevel},
		{"off", zerolog.Disabled},
		{"none", zerolog.Disabled},
	}
	for _, c := range cases {
		got, err := ParseLevel(c.in)
		if err != nil {
			t.Fatalf("ParseLevel(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	if _, err := ParseLevel("bogus"); err == nil {
		t.Error("ParseLevel(bogus) should error")
	}
}

func TestNewLoggerRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, zerolog.WarnLevel)
	log.Info().Msg("hidden")
	log.Warn().Msg("shown")
	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Errorf("info should be filtered at warn level: %s", out)
	}
	if !strings.Contains(out, "shown") {
		t.Errorf("warn should be emitted: %s", out)
	}
}
