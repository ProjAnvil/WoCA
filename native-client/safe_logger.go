package main

import (
	"strings"
	"unicode"

	wailslogger "github.com/wailsapp/wails/v2/pkg/logger"
)

const redactedDispatcherCallError = "process message error: bound call details redacted"

type redactingLogger struct {
	delegate wailslogger.Logger
}

func newRedactingLogger(delegate wailslogger.Logger) *redactingLogger {
	if delegate == nil {
		delegate = wailslogger.NewDefaultLogger()
	}
	return &redactingLogger{delegate: delegate}
}

func (l *redactingLogger) Print(message string)   { l.delegate.Print(sanitizeWailsLog(message)) }
func (l *redactingLogger) Trace(message string)   { l.delegate.Trace(sanitizeWailsLog(message)) }
func (l *redactingLogger) Debug(message string)   { l.delegate.Debug(sanitizeWailsLog(message)) }
func (l *redactingLogger) Info(message string)    { l.delegate.Info(sanitizeWailsLog(message)) }
func (l *redactingLogger) Warning(message string) { l.delegate.Warning(sanitizeWailsLog(message)) }
func (l *redactingLogger) Error(message string)   { l.delegate.Error(sanitizeWailsLog(message)) }
func (l *redactingLogger) Fatal(message string)   { l.delegate.Fatal(sanitizeWailsLog(message)) }

func sanitizeWailsLog(message string) string {
	// Wails v2.14 formats dispatcher failures with the complete IPC message,
	// including bound-call arguments, before invoking the public logger. Both
	// named calls (C) and obfuscated secure calls (c) must therefore lose the
	// entire payload and error detail at this final application-owned boundary.
	const marker = "process message error:"
	markerIndex := strings.Index(message, marker)
	if markerIndex < 0 {
		return message
	}
	payload := strings.TrimLeftFunc(message[markerIndex+len(marker):], unicode.IsSpace)
	if payload != "" && (payload[0] == 'C' || payload[0] == 'c') {
		return redactedDispatcherCallError
	}
	return message
}

var _ wailslogger.Logger = (*redactingLogger)(nil)
