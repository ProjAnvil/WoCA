package logging

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

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
