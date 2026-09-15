package main

import (
	"testing"

	"github.com/rs/zerolog"
)

func TestWorkerHelp(t *testing.T) {
	// The worker requires a shared database; calling it without one errors.
	root := newWorkerRoot()
	root.SetArgs([]string{"--db", "none"})
	root.SilenceUsage = true
	root.SilenceErrors = true
	if err := root.Execute(); err == nil {
		t.Fatal("worker without a database succeeded")
	}
}

func TestWorkerBadLogLevel(t *testing.T) {
	root := newWorkerRoot()
	root.SetArgs([]string{"--log-level", "bogus"})
	root.SilenceUsage = true
	root.SilenceErrors = true
	if err := root.Execute(); err == nil {
		t.Fatal("bogus log level accepted")
	}
}

func TestServeProbesOff(t *testing.T) {
	stop, err := serveProbes(t.Context(), "off", zerolog.Nop(), nil)
	if err != nil {
		t.Fatalf("serveProbes(off) err = %v", err)
	}
	stop() // no-op for the disabled posture
}
