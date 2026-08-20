package main

import (
	"testing"
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
