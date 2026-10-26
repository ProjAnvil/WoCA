package main

// Entry-point tests follow the worker main's pattern: the boot-order
// error paths (no database, bad log level, plain build without a backend
// switch) plus table tests for the pure helpers (hardening env, label
// builder, advertise resolution). The full assembly needs nacos and a
// container backend — that is the lab/e2e plane's job (Task 17/18), not
// a unit test's.

import (
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/ee/executor"
)

func TestExecutorNoDB(t *testing.T) {
	// The executor requires a shared database; "none" errors before
	// anything else touches the network or the daemon.
	root := newExecutorRoot()
	root.SetArgs([]string{"--db", "none", "--obs-addr", "off"})
	root.SilenceUsage = true
	root.SilenceErrors = true
	if err := root.Execute(); err == nil {
		t.Fatal("executor without a database succeeded")
	}
}

func TestExecutorBadLogLevel(t *testing.T) {
	root := newExecutorRoot()
	root.SetArgs([]string{"--log-level", "bogus", "--obs-addr", "off"})
	root.SilenceUsage = true
	root.SilenceErrors = true
	if err := root.Execute(); err == nil {
		t.Fatal("bogus log level accepted")
	}
}

// TestExecutorPlainBuildNeedsBackend pins the build matrix: in plain
// builds (no dockerlab tag) there is no production backend, and boot
// must refuse rather than silently degrade. With the development switch
// set, boot proceeds past the backend step (and stops at the next
// hard requirement — the missing nacos registry or database).
func TestExecutorPlainBuildNeedsBackend(t *testing.T) {
	if backendFactory != nil {
		t.Skip("dockerlab build registers a backend factory")
	}
	t.Setenv(fakeBackendEnv, "")
	root := newExecutorRoot()
	root.SetArgs([]string{"--db", "sqlite:" + t.TempDir() + "/e.db", "--obs-addr", "off"})
	root.SilenceUsage = true
	root.SilenceErrors = true
	err := root.Execute()
	if err == nil {
		t.Fatal("plain build without WOCA_DEV_FAKE_BACKEND started")
	}
	if !strings.Contains(err.Error(), "no container backend") {
		t.Fatalf("err = %v, want it to mention %q", err, "no container backend")
	}
}

func TestHardeningFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		want    executor.Hardening
		wantErr bool
	}{
		{"empty defaults full", "", executor.HardeningFull, false},
		{"full", "full", executor.HardeningFull, false},
		{"minimal", "minimal", executor.HardeningMinimal, false},
		{"case-insensitive", "Minimal", executor.HardeningMinimal, false},
		{"unknown", "yolo", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(hardeningEnv, tt.env)
			got, err := hardeningFromEnv()
			if (err != nil) != tt.wantErr {
				t.Fatalf("hardeningFromEnv() err = %v wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("hardeningFromEnv() = %v want %v", got, tt.want)
			}
		})
	}
}

func TestLabelsFor(t *testing.T) {
	f := labelsFor("executor-h1")
	routed := f("ws-abc")
	if routed[executor.LabelSandbox] != "1" {
		t.Fatalf("sandbox marker missing: %v", routed)
	}
	if routed[executor.LabelExecutor] != "executor-h1" {
		t.Fatalf("executor label = %q", routed[executor.LabelExecutor])
	}
	if routed[executor.LabelWorkspace] != "ws-abc" {
		t.Fatalf("workspace label = %q", routed[executor.LabelWorkspace])
	}
	if _, ok := routed[executor.LabelRun]; ok {
		t.Fatalf("run label must stay off (no run identity at this layer): %v", routed)
	}
	unrouted := f("")
	if _, ok := unrouted[executor.LabelWorkspace]; ok {
		t.Fatalf("unrouted create must not carry a workspace label: %v", unrouted)
	}
}

func TestAdvertiseAddr(t *testing.T) {
	tests := []struct {
		name      string
		advertise string
		listen    string
		env       string
		want      string
	}{
		{"explicit", "box1:9000", ":8093", "", "box1:9000"},
		{"explicit with scheme", "http://box1:9000", ":8093", "", "box1:9000"},
		{"explicit bare host takes listen port", "box1", "0.0.0.0:8093", "", "box1:8093"},
		{"env wins over flag", "box1:9000", ":8093", "box2:7000", "box2:7000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env == "" {
				t.Setenv(advertiseAddrEnv, "")
			} else {
				t.Setenv(advertiseAddrEnv, tt.env)
			}
			if got := advertiseAddr(tt.advertise, tt.listen); got != tt.want {
				t.Fatalf("advertiseAddr(%q, %q) = %q want %q", tt.advertise, tt.listen, got, tt.want)
			}
		})
	}
}

// TestAdvertiseAddrDefault derives from the hostname when nothing is
// configured: host name (short form) + the listen port.
func TestAdvertiseAddrDefault(t *testing.T) {
	t.Setenv(advertiseAddrEnv, "")
	got := advertiseAddr("", ":8093")
	wantSuffix := ":8093"
	if len(got) < len(wantSuffix) || got[len(got)-len(wantSuffix):] != wantSuffix {
		t.Fatalf("advertiseAddr default = %q, want it to end with the listen port", got)
	}
	if got == wantSuffix || got == "localhost"+wantSuffix {
		// localhost fallback is acceptable only when Hostname() failed,
		// which is not the case in a test environment.
		t.Fatalf("advertiseAddr default = %q, want the host's name", got)
	}
}
