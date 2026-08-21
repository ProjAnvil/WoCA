package main

import (
	wailslogger "github.com/wailsapp/wails/v2/pkg/logger"
)

const redactedWailsMessage = "Wails runtime message redacted"

type redactingLogger struct {
	delegate wailslogger.Logger
}

func newRedactingLogger(delegate wailslogger.Logger) *redactingLogger {
	if delegate == nil {
		delegate = wailslogger.NewDefaultLogger()
	}
	return &redactingLogger{delegate: delegate}
}

// Wails v2.14 routes dispatcher arguments, callback results, frontend
// runtime.Log* text, and internal diagnostics through the same public Logger
// interface. Their origin is no longer distinguishable here, so every message
// body is treated as untrusted and discarded. Calling the matching delegate
// method retains severity and, for Fatal, the delegate's termination semantics.
func (l *redactingLogger) Print(string)   { l.delegate.Print(redactedWailsMessage) }
func (l *redactingLogger) Trace(string)   { l.delegate.Trace(redactedWailsMessage) }
func (l *redactingLogger) Debug(string)   { l.delegate.Debug(redactedWailsMessage) }
func (l *redactingLogger) Info(string)    { l.delegate.Info(redactedWailsMessage) }
func (l *redactingLogger) Warning(string) { l.delegate.Warning(redactedWailsMessage) }
func (l *redactingLogger) Error(string)   { l.delegate.Error(redactedWailsMessage) }
func (l *redactingLogger) Fatal(string)   { l.delegate.Fatal(redactedWailsMessage) }

var _ wailslogger.Logger = (*redactingLogger)(nil)
