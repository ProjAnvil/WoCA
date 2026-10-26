package main

// Boot-order error paths (the worker main's pattern): the proxy refuses
// to start without the shared database; the rest of its surface is the
// proxy package's own tests.

import (
	"testing"
)

func TestProxyNoDB(t *testing.T) {
	root := newProxyRoot()
	root.SetArgs([]string{"--db", "none"})
	root.SilenceUsage = true
	root.SilenceErrors = true
	if err := root.Execute(); err == nil {
		t.Fatal("proxy without a database succeeded")
	}
}

func TestProxyBadLogLevel(t *testing.T) {
	root := newProxyRoot()
	root.SetArgs([]string{"--log-level", "bogus"})
	root.SilenceUsage = true
	root.SilenceErrors = true
	if err := root.Execute(); err == nil {
		t.Fatal("bogus log level accepted")
	}
}
