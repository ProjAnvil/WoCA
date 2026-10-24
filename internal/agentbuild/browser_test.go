package agentbuild

import (
	"errors"
	"slices"
	"testing"
)

func TestOpenBrowserCommand(t *testing.T) {
	t.Parallel()
	const url = "http://127.0.0.1:1/x?message=hello world&next=/a;b"
	startErr := errors.New("browser command failed to start")
	for _, tt := range []struct {
		goos string
		name string
		args []string
	}{
		{goos: "darwin", name: "open", args: []string{url}},
		{goos: "linux", name: "xdg-open", args: []string{url}},
		{goos: "windows", name: "rundll32", args: []string{"url.dll,FileProtocolHandler", url}},
	} {
		t.Run(tt.goos, func(t *testing.T) {
			t.Parallel()
			for _, result := range []struct {
				name string
				err  error
			}{
				{name: "success"},
				{name: "start failure", err: startErr},
			} {
				t.Run(result.name, func(t *testing.T) {
					t.Parallel()
					var calls int
					var gotName string
					var gotArgs []string
					start := func(name string, args ...string) error {
						calls++
						gotName, gotArgs = name, slices.Clone(args)
						return result.err
					}

					err := openBrowserOnOS(tt.goos, url, start)
					if !errors.Is(err, result.err) {
						t.Fatalf("openBrowserOnOS() error = %v, want %v", err, result.err)
					}
					if calls != 1 {
						t.Fatalf("start called %d times, want 1", calls)
					}
					if gotName != tt.name || !slices.Equal(gotArgs, tt.args) {
						t.Errorf("start(%q, %q), want start(%q, %q)", gotName, gotArgs, tt.name, tt.args)
					}
				})
			}
		})
	}
}

func TestOpenBrowserUnsupported(t *testing.T) {
	t.Parallel()
	calls := 0
	err := openBrowserOnOS("plan9", "http://127.0.0.1:1/x", func(string, ...string) error {
		calls++
		return nil
	})
	if err == nil || err.Error() != `unsupported platform "plan9"` {
		t.Errorf("openBrowserOnOS() error = %v, want unsupported platform error", err)
	}
	if calls != 0 {
		t.Errorf("start called %d times on unsupported platform, want 0", calls)
	}
}
