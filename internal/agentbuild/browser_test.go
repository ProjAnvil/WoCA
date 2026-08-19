package agentbuild

import "testing"

func TestOpenBrowserUnsupported(t *testing.T) {
	// On darwin/linux the command starts; cmd.Start itself rarely fails, so
	// only assert no panic.
	_ = openBrowser("http://127.0.0.1:1/x")
}
