package webgui

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/server"
)

func TestDefaults(t *testing.T) {
	if DefaultPort != 3484 {
		t.Fatalf("DefaultPort = %d, want 3484", DefaultPort)
	}
	if DefaultAddr != "127.0.0.1:3484" {
		t.Fatalf("DefaultAddr = %q, want 127.0.0.1:3484", DefaultAddr)
	}
}

func TestManagerStartStop(t *testing.T) {
	m := New(server.Config{Logger: zerolog.Nop()})

	if _, running := m.Running(); running {
		t.Fatal("manager should not be running before Start")
	}

	url, err := m.Start("127.0.0.1:0") // ephemeral port for test isolation
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if url == "" || url == "http://" {
		t.Fatalf("Start returned empty URL %q", url)
	}
	addr, running := m.Running()
	if !running {
		t.Fatal("manager should be running after Start")
	}

	// The health endpoint answers through the started server.
	resp, err := http.Get("http://" + addr + "/api/health")
	if err != nil {
		t.Fatalf("GET /api/health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if body["ok"] != true {
		t.Fatalf("health body = %v", body)
	}

	// Starting again while running is an error.
	if _, err := m.Start("127.0.0.1:0"); err == nil {
		t.Fatal("second Start should fail while running")
	}

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, running = m.Running(); running {
		t.Fatal("manager should not be running after Stop")
	}

	// After shutdown the endpoint no longer answers.
	time.Sleep(50 * time.Millisecond)
	if _, err := http.Get("http://" + addr + "/api/health"); err == nil {
		t.Fatal("server should be closed after Stop")
	}
}

func TestManagerStartPortInUse(t *testing.T) {
	m1 := New(server.Config{Logger: zerolog.Nop()})
	url, err := m1.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("m1 Start: %v", err)
	}
	defer m1.Stop()

	// Extract the concrete address and try to bind it again.
	addr := url[len("http://"):]
	m2 := New(server.Config{Logger: zerolog.Nop()})
	if _, err := m2.Start(addr); err == nil {
		defer m2.Stop()
		t.Fatal("Start on a taken port should fail")
	}
}

func TestManagerStopWhenNotRunning(t *testing.T) {
	m := New(server.Config{Logger: zerolog.Nop()})
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop on idle manager = %v, want nil", err)
	}
}
