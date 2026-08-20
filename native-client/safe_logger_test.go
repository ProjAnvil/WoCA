package main

import (
	"fmt"
	"strings"
	"testing"

	wailslogger "github.com/wailsapp/wails/v2/pkg/logger"
)

type logEntry struct {
	level   string
	message string
}

type recordingLogger struct {
	entries []logEntry
}

func (l *recordingLogger) record(level, message string) {
	l.entries = append(l.entries, logEntry{level: level, message: message})
}

func (l *recordingLogger) Print(message string)   { l.record("print", message) }
func (l *recordingLogger) Trace(message string)   { l.record("trace", message) }
func (l *recordingLogger) Debug(message string)   { l.record("debug", message) }
func (l *recordingLogger) Info(message string)    { l.record("info", message) }
func (l *recordingLogger) Warning(message string) { l.record("warning", message) }
func (l *recordingLogger) Error(message string)   { l.record("error", message) }
func (l *recordingLogger) Fatal(message string)   { l.record("fatal", message) }

func TestRedactingLoggerSanitizesDispatcherCallPayloadOnEveryMethod(t *testing.T) {
	const secret = "unique-secret-api-key"
	const serverURL = "https://private-enterprise.example.com"
	message := fmt.Sprintf(
		`process message error: C{"name":"main.App.Connect","args":[{"server_url":%q,"api_key":%q}]} -> cannot decode %s`,
		serverURL,
		secret,
		secret,
	)
	recorder := &recordingLogger{}
	logger := newRedactingLogger(recorder)

	methods := []struct {
		level string
		call  func(string)
	}{
		{level: "print", call: logger.Print},
		{level: "trace", call: logger.Trace},
		{level: "debug", call: logger.Debug},
		{level: "info", call: logger.Info},
		{level: "warning", call: logger.Warning},
		{level: "error", call: logger.Error},
		{level: "fatal", call: logger.Fatal},
	}
	for _, method := range methods {
		method.call(message)
	}

	if len(recorder.entries) != len(methods) {
		t.Fatalf("entries = %d, want %d", len(recorder.entries), len(methods))
	}
	for index, entry := range recorder.entries {
		if entry.level != methods[index].level || entry.message != redactedDispatcherCallError {
			t.Fatalf("entry %d = %#v", index, entry)
		}
		if strings.Contains(entry.message, secret) || strings.Contains(entry.message, serverURL) || strings.Contains(entry.message, `"args"`) {
			t.Fatalf("entry %d leaked dispatcher payload: %q", index, entry.message)
		}
	}
}

func TestRedactingLoggerSanitizesObfuscatedSecureCallPanicAndDelegatesFatal(t *testing.T) {
	const secret = "obfuscated-panic-secret"
	recorder := &recordingLogger{}
	logger := newRedactingLogger(recorder)

	logger.Fatal(`process message error: c{"id":91,"args":[{"value":"` + secret + `"}]} -> panic: ` + secret)

	if len(recorder.entries) != 1 {
		t.Fatalf("entries = %#v", recorder.entries)
	}
	if recorder.entries[0] != (logEntry{level: "fatal", message: redactedDispatcherCallError}) {
		t.Fatalf("Fatal entry = %#v", recorder.entries[0])
	}
}

func TestRedactingLoggerDelegatesUnrelatedMessagesUnchanged(t *testing.T) {
	recorder := &recordingLogger{}
	logger := newRedactingLogger(recorder)

	logger.Info("ordinary Wails startup message")

	if len(recorder.entries) != 1 || recorder.entries[0] != (logEntry{
		level: "info", message: "ordinary Wails startup message",
	}) {
		t.Fatalf("entries = %#v", recorder.entries)
	}
}

var _ wailslogger.Logger = (*recordingLogger)(nil)
