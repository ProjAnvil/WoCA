package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// execRelayCmd runs the woca-relay root command with args, returning the
// combined failure (nil on success) — the cmd/woca cli_test.go pattern.
func execRelayCmd(t *testing.T, args ...string) error {
	t.Helper()
	return execRelayCmdCtx(t, context.Background(), args...)
}

func execRelayCmdCtx(t *testing.T, ctx context.Context, args ...string) error {
	t.Helper()
	root := newRootCmd()
	root.SetOut(os.Stderr)
	root.SetErr(os.Stderr)
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}

// tempDB returns a database path inside a fresh temp dir.
func tempDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "relay.db")
}

func TestRootHelp(t *testing.T) {
	if err := execRelayCmd(t, "--help"); err != nil {
		t.Fatal(err)
	}
	if err := execRelayCmd(t, "key", "--help"); err != nil {
		t.Fatal(err)
	}
	if err := execRelayCmd(t, "serve", "--help"); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultDBPath(t *testing.T) {
	t.Setenv("WOCA_RELAY_DB", "/custom/relay.db")
	if got := defaultDBPath(); got != "/custom/relay.db" {
		t.Fatalf("env override: %q", got)
	}

	home := t.TempDir()
	t.Setenv("WOCA_RELAY_DB", "")
	t.Setenv("HOME", home)
	want := filepath.Join(home, ".woca-relay", "relay.db")
	if got := defaultDBPath(); got != want {
		t.Fatalf("home-derived default: %q, want %q", got, want)
	}

	// No HOME at all: the bare relative fallback.
	t.Setenv("HOME", "")
	if got := defaultDBPath(); got != "woca-relay.db" {
		t.Fatalf("homeless default: %q", got)
	}
}

func TestOpenKeysError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ks, err := openKeys(filepath.Join(blocker, "relay.db"))
	if err == nil {
		t.Fatal("open under a file must fail")
	}
	if ks != nil {
		t.Fatal("failed open returned a store")
	}
	// The CLI surfaces the open error rather than a panic.
	if err := execRelayCmd(t, "--db", filepath.Join(blocker, "relay.db"), "key", "list"); err == nil ||
		!strings.Contains(err.Error(), "relay:") {
		t.Fatalf("key list with unopenable db: %v", err)
	}
}

func TestMainSuccess(t *testing.T) {
	// main() on a successful invocation returns without exiting.
	old := os.Args
	os.Args = []string{"woca-relay", "--help"}
	defer func() { os.Args = old }()
	main()
}

func TestMainFailureExits(t *testing.T) {
	// main() on a failed invocation prints the error and exits 1 — run as
	// a subprocess so the exit does not kill the test binary (the standard
	// helper-process pattern).
	if os.Getenv("WOCA_RELAY_TEST_MAIN_HELPER") == "1" {
		old := os.Args
		os.Args = []string{"woca-relay", "--db", os.Getenv("WOCA_RELAY_TEST_DB"), "key", "list"}
		defer func() { os.Args = old }()
		main()
		return
	}
	// The db's parent is a regular file: open must fail.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^TestMainFailureExits$")
	cmd.Env = append(os.Environ(),
		"WOCA_RELAY_TEST_MAIN_HELPER=1",
		"WOCA_RELAY_TEST_DB="+filepath.Join(blocker, "relay.db"))
	err := cmd.Run()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("helper exit: %v (want exit code 1)", err)
	}
}
