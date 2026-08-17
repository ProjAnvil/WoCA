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

func TestParseLevelAllValues(t *testing.T) {
	for _, c := range []struct {
		v    string
		want zerolog.Level
		ok   bool
	}{
		{"trace", zerolog.TraceLevel, true},
		{"debug", zerolog.DebugLevel, true},
		{"info", zerolog.InfoLevel, true},
		{"warn", zerolog.WarnLevel, true},
		{"error", zerolog.ErrorLevel, true},
		{"disabled", zerolog.Disabled, true},
		{"WARN", zerolog.WarnLevel, true},
		{"bogus", 0, false},
	} {
		got, err := ParseLevel(c.v)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("ParseLevel(%q) = %v %v", c.v, got, err)
		}
		if !c.ok && err == nil {
			t.Errorf("ParseLevel(%q) should fail", c.v)
		}
	}
}

func TestNewAndNewConsole(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, zerolog.InfoLevel)
	log.Info().Str("k", "v").Msg("hello-json")
	if !strings.Contains(buf.String(), "hello-json") {
		t.Fatalf("json log = %q", buf.String())
	}

	var cbuf bytes.Buffer
	clog := NewConsole(&cbuf, zerolog.InfoLevel)
	clog.Info().Str("k", "v").Msg("hello-console")
	if !strings.Contains(cbuf.String(), "hello-console") {
		t.Fatalf("console log = %q", cbuf.String())
	}
	// Below-level records are dropped.
	cbuf.Reset()
	clog.Debug().Msg("dropped")
	if cbuf.Len() != 0 {
		t.Fatalf("debug should be dropped: %q", cbuf.String())
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
